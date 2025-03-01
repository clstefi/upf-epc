// SPDX-License-Identifier: Apache-2.0
// Copyright(c) 2020 Intel Corporation

package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	pb "github.com/omec-project/upf-epc/pfcpiface/bess_pb"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/wmnsk/go-pfcp/ie"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// DefaultBurstSize for cbs, pbs required for dpdk metering. 32 MTUs.
	DefaultBurstSize = 32 * 1514
	// SockAddr : Unix Socket path to read bess notification from.
	SockAddr = "/tmp/notifycp"
	// PfcpAddr : Unix Socket path to send end marker packet.
	PfcpAddr = "/tmp/pfcpport"
	// AppQerLookup: Application Qos table Name.
	AppQerLookup = "appQERLookup"
	// SessQerLookup: Session Qos table Name.
	SessQerLookup = "sessionQERLookup"
	// PreQosFlowMeasure: Pre QoS measurement module name.
	PreQosFlowMeasure = "preQosFlowMeasure"
	// PostDlQosFlowMeasure: Post QoS measurement downlink module name.
	PostDlQosFlowMeasure = "postDLQosFlowMeasure"
	// PostUlQosFlowMeasure: Post QoS measurement uplink module name.
	PostUlQosFlowMeasure = "postULQosFlowMeasure"
	// far-action specific values.
	farForwardD = 0x0
	farForwardU = 0x1
	farDrop     = 0x2
	farBuffer   = 0x3
	farNotify   = 0x4
	// Bit Rates.
	KB = 1000
	MB = 1000000
	GB = 1000000000
)

const (
	// Internal gates for QER.
	qerGateMeter      uint64 = iota
	qerGateStatusDrop        = iota + 4
	qerGateUnmeter
)

const (
	// Internal gates for Slice meter.
	sliceMeterGateMeter      uint64 = iota
	sliceMeterGateLookupFail        = iota + 4
	sliceMeterGateUnmeter
)

var intEnc = func(u uint64) *pb.FieldData {
	return &pb.FieldData{Encoding: &pb.FieldData_ValueInt{ValueInt: u}}
}

var bessIP = flag.String("bess", "localhost:10514", "BESS IP/port combo")

type bess struct {
	client           pb.BESSControlClient
	conn             *grpc.ClientConn
	endMarkerSocket  net.Conn
	notifyBessSocket net.Conn
	endMarkerChan    chan []byte
	qciQosMap        map[uint8]*QosConfigVal
}

func (b *bess) isConnected(accessIP *net.IP) bool {
	if (b.conn == nil) || (int(b.conn.GetState()) != Ready) {
		return false
	}

	return true
}

func (b *bess) sendEndMarkers(endMarkerList *[][]byte) error {
	for _, eMarker := range *endMarkerList {
		b.endMarkerChan <- eMarker
	}

	return nil
}

func (b *bess) addSliceInfo(sliceInfo *SliceInfo) error {
	var sliceMeterConfig SliceMeterConfig
	sliceMeterConfig.N6RateBps = sliceInfo.uplinkMbr
	sliceMeterConfig.N3RateBps = sliceInfo.downlinkMbr
	sliceMeterConfig.N6BurstBytes = sliceInfo.ulBurstBytes
	sliceMeterConfig.N3BurstBytes = sliceInfo.dlBurstBytes

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	done := make(chan bool)

	b.addSliceMeter(ctx, done, sliceMeterConfig)

	rc := b.GRPCJoin(1, Timeout, done)

	if !rc {
		log.Errorln("Unable to make GRPC calls")
	}

	return nil
}

func (b *bess) sendMsgToUPF(
	method upfMsgType, pdrs []pdr, fars []far, qers []qer) uint8 {
	// create context
	var cause uint8 = ie.CauseRequestAccepted

	calls := len(pdrs) + len(fars) + len(qers)
	if calls == 0 {
		return cause
	}

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	done := make(chan bool)

	for _, pdr := range pdrs {
		log.Traceln(pdr)

		switch method {
		case upfMsgTypeAdd:
			fallthrough
		case upfMsgTypeMod:
			b.addPDR(ctx, done, pdr)
		case upfMsgTypeDel:
			b.delPDR(ctx, done, pdr)
		}
	}

	for _, far := range fars {
		log.Traceln(far)

		switch method {
		case upfMsgTypeAdd:
			fallthrough
		case upfMsgTypeMod:
			b.addFAR(ctx, done, far)
		case upfMsgTypeDel:
			b.delFAR(ctx, done, far)
		}
	}

	for _, qer := range qers {
		log.Traceln("qer:", qer)

		switch method {
		case upfMsgTypeAdd:
			fallthrough
		case upfMsgTypeMod:
			b.addQER(ctx, done, qer)
		case upfMsgTypeDel:
			b.delQER(ctx, done, qer)
		}
	}

	rc := b.GRPCJoin(calls, Timeout, done)
	if !rc {
		log.Println("Unable to make GRPC calls")
	}

	return cause
}

func (b *bess) exit() {
	log.Println("Exit function Bess")
	b.conn.Close()
}

func (b *bess) measureUpf(ifName string, f *pb.MeasureCommandGetSummaryArg) *pb.MeasureCommandGetSummaryResponse {
	modName := func() string {
		return ifName + "_measure"
	}

	any, err := anypb.New(f)
	if err != nil {
		log.Println("Error marshalling the rule", f, err)
		return nil
	}

	ctx := context.Background()

	modRes, err := b.client.ModuleCommand(ctx, &pb.CommandRequest{
		Name: modName(),
		Cmd:  "get_summary",
		Arg:  any,
	})
	if err != nil {
		log.Println("Error calling get_summary on module", modName(), err)
		return nil
	}

	var res pb.MeasureCommandGetSummaryResponse

	err = modRes.GetData().UnmarshalTo(&res)
	if err != nil {
		log.Println("Error unmarshalling the response", modName(), err)
		return nil
	}

	return &res
}

func (b *bess) getPortStats(ifname string) *pb.GetPortStatsResponse {
	ctx := context.Background()
	req := &pb.GetPortStatsRequest{
		Name: ifname + "Fast",
	}

	res, err := b.client.GetPortStats(ctx, req)
	if err != nil {
		log.Println("Error calling GetPortStats", ifname, err)
		return nil
	}
	if res.GetError() != nil {
		log.Println("Error calling GetPortStats", ifname, err, res.GetError().Errmsg)
		return nil
	}

	return res
}

func (b *bess) portStats(uc *upfCollector, ch chan<- prometheus.Metric) {
	portstats := func(ifaceLabel, ifaceName string) {
		packets := func(packets uint64, direction string) {
			p := prometheus.MustNewConstMetric(
				uc.packets,
				prometheus.CounterValue,
				float64(packets),
				ifaceLabel, direction,
			)
			ch <- p
		}
		bytes := func(bytes uint64, direction string) {
			p := prometheus.MustNewConstMetric(
				uc.bytes,
				prometheus.CounterValue,
				float64(bytes),
				ifaceLabel, direction,
			)
			ch <- p
		}
		dropped := func(dropped uint64, direction string) {
			p := prometheus.MustNewConstMetric(
				uc.dropped,
				prometheus.CounterValue,
				float64(dropped),
				ifaceLabel, direction,
			)
			ch <- p
		}

		res := b.getPortStats(ifaceName)
		if res == nil {
			return
		}

		packets(res.Inc.Packets, "rx")
		packets(res.Out.Packets, "tx")

		bytes(res.Inc.Bytes, "rx")
		bytes(res.Out.Bytes, "tx")

		dropped(res.Inc.Dropped, "rx")
		dropped(res.Out.Dropped, "tx")
	}

	portstats("Access", uc.upf.accessIface)
	portstats("Core", uc.upf.coreIface)
}

func (b *bess) summaryLatencyJitter(uc *upfCollector, ch chan<- prometheus.Metric) {
	measureIface := func(ifaceLabel, ifaceName string) {
		req := &pb.MeasureCommandGetSummaryArg{
			Clear:              true,
			LatencyPercentiles: getPctiles(),
			JitterPercentiles:  getPctiles(),
		}

		res := b.measureUpf(ifaceName, req)
		if res == nil {
			return
		}

		latencies := res.GetLatency().GetPercentileValuesNs()
		if latencies != nil {
			l := prometheus.MustNewConstSummary(
				uc.latency,
				res.Packets,
				float64(res.Latency.GetTotalNs()),
				makeBuckets(latencies),
				ifaceLabel,
			)

			ch <- l
		}

		jitters := res.GetJitter().GetPercentileValuesNs()
		if jitters != nil {
			j := prometheus.MustNewConstSummary(
				uc.jitter,
				res.Packets,
				float64(res.Jitter.GetTotalNs()),
				makeBuckets(jitters),
				ifaceLabel,
			)

			ch <- j
		}
	}
	measureIface("Access", uc.upf.accessIface)
	measureIface("Core", uc.upf.coreIface)
}

func (b *bess) readFlowMeasurement(
	ctx context.Context, module string, clear bool, q []float64,
) (stats pb.FlowMeasureReadResponse, err error) {
	req := &pb.FlowMeasureCommandReadArg{
		Clear:              clear,
		LatencyPercentiles: q,
		JitterPercentiles:  q,
	}
	any, err := anypb.New(req)
	if err != nil {
		log.Errorln("Error marshalling request", req, err)
		return
	}
	resp, err := b.client.ModuleCommand(
		ctx, &pb.CommandRequest{
			Name: module,
			Cmd:  "read",
			Arg:  any,
		},
	)
	if err != nil {
		log.Errorln(module, "read failed!:", err)
		return
	}
	if err = resp.Data.UnmarshalTo(&stats); err != nil {
		log.Errorln(err)
		return
	}
	return
}

func (b *bess) sessionStats(uc *upfCollector, ch chan<- prometheus.Metric) (err error) {
	// Clearing table data with large tables is slow, let's wait for a little longer since this is
	// non-blocking for the dataplane anyway.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	// Read stats. This flips the buffer flag, reads from the now inactive side, and clear if needed.
	q := []float64{50, 90, 99}
	qosStatsInResp, err := b.readFlowMeasurement(ctx, PreQosFlowMeasure, true, q)
	if err != nil {
		log.Errorln(PreQosFlowMeasure, " read failed!:", err)
		return
	}
	postDlQosStatsResp, err := b.readFlowMeasurement(ctx, PostDlQosFlowMeasure, true, q)
	if err != nil {
		log.Errorln(PostDlQosFlowMeasure, " read failed!:", err)
		return
	}
	postUlQosStatsResp, err := b.readFlowMeasurement(ctx, PostUlQosFlowMeasure, true, q)
	if err != nil {
		log.Errorln(PostUlQosFlowMeasure, " read failed!:", err)
		return
	}

	// Prepare prometheus stats.
	createStats := func(preResp, postResp *pb.FlowMeasureReadResponse, ch chan<- prometheus.Metric) {
		for i := 0; i < len(postResp.Statistics); i++ {
			post := postResp.Statistics[i]
			var pre *pb.FlowMeasureReadResponse_Statistic
			// Find preQos values.
			for _, v := range preResp.Statistics {
				if post.Pdr == v.Pdr && post.Fseid == v.Fseid {
					pre = v
					break
				}
			}
			if pre == nil {
				log.Infof("Found no pre QoS statistics for PDR %v FSEID %v", post.Pdr, post.Fseid)
				continue
			}

			fseidString := strconv.FormatUint(post.Fseid, 10)
			pdrString := strconv.FormatUint(post.Pdr, 10)
			ch <- prometheus.MustNewConstMetric(
				uc.sessionTxPackets,
				prometheus.GaugeValue,
				float64(post.TotalPackets),
				fseidString,
				pdrString,
			)
			ch <- prometheus.MustNewConstMetric(
				uc.sessionDroppedPackets,
				prometheus.GaugeValue,
				float64(pre.TotalPackets-post.TotalPackets),
				fseidString,
				pdrString,
			)
			ch <- prometheus.MustNewConstMetric(
				uc.sessionTxBytes,
				prometheus.GaugeValue,
				float64(post.TotalBytes),
				fseidString,
				pdrString,
			)
			ch <- prometheus.MustNewConstSummary(
				uc.sessionLatency,
				post.TotalPackets,
				0,
				map[float64]float64{
					50.0: float64(post.Latency.PercentileValuesNs[0]),
					99.0: float64(post.Latency.PercentileValuesNs[1]),
					99.9: float64(post.Latency.PercentileValuesNs[2]),
				},
				fseidString,
				pdrString,
			)
			ch <- prometheus.MustNewConstSummary(
				uc.sessionJitter,
				post.TotalPackets,
				0,
				map[float64]float64{
					50.0: float64(post.Jitter.PercentileValuesNs[0]),
					99.0: float64(post.Jitter.PercentileValuesNs[1]),
					99.9: float64(post.Jitter.PercentileValuesNs[2]),
				},
				fseidString,
				pdrString,
			)
		}
	}

	createStats(&qosStatsInResp, &postUlQosStatsResp, ch)
	createStats(&qosStatsInResp, &postDlQosStatsResp, ch)

	return
}

func (b *bess) endMarkerSendLoop(endMarkerChan chan []byte) {
	for outPacket := range endMarkerChan {
		_, err := b.endMarkerSocket.Write(outPacket)
		if err != nil {
			log.Println("end marker write failed")
		}
	}
}

func (b *bess) notifyListen(reportNotifyChan chan<- uint64) {
	for {
		buf := make([]byte, 512)

		_, err := b.notifyBessSocket.Read(buf)
		if err != nil {
			return
		}

		d := buf[0:8]
		fseid := binary.LittleEndian.Uint64(d)
		reportNotifyChan <- fseid
	}
}

func (b *bess) readQciQosMap(conf *Conf) {
	b.qciQosMap = make(map[uint8]*QosConfigVal)

	for _, qosVal := range conf.QciQosConfig {
		qosConfigVal := &QosConfigVal{
			cbs:              qosVal.CBS,
			ebs:              qosVal.EBS,
			pbs:              qosVal.PBS,
			burstDurationMs:  qosVal.BurstDurationMs,
			schedulePriority: qosVal.SchedulingPriority,
		}
		b.qciQosMap[qosVal.QCI] = qosConfigVal
	}

	if _, ok := b.qciQosMap[0]; !ok {
		b.qciQosMap[0] = &QosConfigVal{
			cbs:              DefaultBurstSize,
			ebs:              DefaultBurstSize,
			pbs:              DefaultBurstSize,
			burstDurationMs:  10,
			schedulePriority: 7,
		}
	}
}

func (b *bess) setUpfInfo(u *upf, conf *Conf) {
	var err error

	log.Println("setUpfInfo bess")

	b.readQciQosMap(conf)
	// get bess grpc client
	log.Println("bessIP ", *bessIP)

	b.endMarkerChan = make(chan []byte, 1024)

	b.conn, err = grpc.Dial(*bessIP, grpc.WithInsecure())
	if err != nil {
		log.Fatalln("did not connect:", err)
	}

	b.client = pb.NewBESSControlClient(b.conn)

	if conf.EnableNotifyBess {
		notifySockAddr := conf.NotifySockAddr
		if notifySockAddr == "" {
			notifySockAddr = SockAddr
		}

		b.notifyBessSocket, err = net.Dial("unixpacket", notifySockAddr)
		if err != nil {
			log.Println("dial error:", err)
			return
		}

		go b.notifyListen(u.reportNotifyChan)
	}

	if conf.EnableEndMarker {
		pfcpCommAddr := conf.EndMarkerSockAddr
		if pfcpCommAddr == "" {
			pfcpCommAddr = PfcpAddr
		}

		b.endMarkerSocket, err = net.Dial("unixpacket", pfcpCommAddr)
		if err != nil {
			log.Println("dial error:", err)
			return
		}

		log.Println("Starting end marker loop")

		go b.endMarkerSendLoop(b.endMarkerChan)
	}

	if (conf.SliceMeterConfig.N6RateBps > 0) ||
		(conf.SliceMeterConfig.N3RateBps > 0) {
		ctx, cancel := context.WithTimeout(context.Background(), Timeout)
		defer cancel()

		done := make(chan bool)

		b.addSliceMeter(ctx, done, conf.SliceMeterConfig)

		rc := b.GRPCJoin(1, Timeout, done)
		if !rc {
			log.Errorln("Unable to make GRPC calls")
		}
	}
}

func (b *bess) processPDR(ctx context.Context, any *anypb.Any, method upfMsgType) {
	if method != upfMsgTypeAdd && method != upfMsgTypeDel && method != upfMsgTypeClear {
		log.Println("Invalid method name: ", method)
		return
	}

	methods := [...]string{"add", "add", "delete", "clear"}

	resp, err := b.client.ModuleCommand(ctx, &pb.CommandRequest{
		Name: "pdrLookup",
		Cmd:  methods[method],
		Arg:  any,
	})

	log.Traceln("pdrlookup resp : ", resp)

	if err != nil || resp.GetError() != nil {
		log.Errorf("pdrLookup method failed with resp: %v, err: %v\n", resp, err)
	}
}

func (b *bess) addPDR(ctx context.Context, done chan<- bool, p pdr) {
	go func() {
		var (
			any *anypb.Any
			err error
		)

		var qerID uint32

		for _, qer := range p.qerIDList {
			qerID = qer
			break
		}

		f := &pb.WildcardMatchCommandAddArg{
			Gate:     uint64(p.needDecap),
			Priority: int64(math.MaxUint32 - p.precedence),
			Values: []*pb.FieldData{
				intEnc(uint64(p.srcIface)),     /* src_iface */
				intEnc(uint64(p.tunnelIP4Dst)), /* tunnel_ipv4_dst */
				intEnc(uint64(p.tunnelTEID)),   /* enb_teid */
				intEnc(uint64(p.srcIP)),        /* ueaddr ip*/
				intEnc(uint64(p.dstIP)),        /* inet ip */
				intEnc(uint64(p.srcPort)),      /* ue port */
				intEnc(uint64(p.dstPort)),      /* inet port */
				intEnc(uint64(p.proto)),        /* proto id */
			},
			Masks: []*pb.FieldData{
				intEnc(uint64(p.srcIfaceMask)),     /* src_iface-mask */
				intEnc(uint64(p.tunnelIP4DstMask)), /* tunnel_ipv4_dst-mask */
				intEnc(uint64(p.tunnelTEIDMask)),   /* enb_teid-mask */
				intEnc(uint64(p.srcIPMask)),        /* ueaddr ip-mask */
				intEnc(uint64(p.dstIPMask)),        /* inet ip-mask */
				intEnc(uint64(p.srcPortMask)),      /* ue port-mask */
				intEnc(uint64(p.dstPortMask)),      /* inet port-mask */
				intEnc(uint64(p.protoMask)),        /* proto id-mask */
			},
			Valuesv: []*pb.FieldData{
				intEnc(uint64(p.pdrID)), /* pdr-id */
				intEnc(p.fseID),         /* fseid */
				intEnc(uint64(p.ctrID)), /* ctr_id */
				intEnc(uint64(qerID)),   /* qer_id */
				intEnc(uint64(p.farID)), /* far_id */
			},
		}

		any, err = anypb.New(f)
		if err != nil {
			log.Println("Error marshalling the rule", f, err)
			return
		}

		b.processPDR(ctx, any, upfMsgTypeAdd)
		done <- true
	}()
}

func (b *bess) delPDR(ctx context.Context, done chan<- bool, p pdr) {
	go func() {
		var (
			any *anypb.Any
			err error
		)

		f := &pb.WildcardMatchCommandDeleteArg{
			Values: []*pb.FieldData{
				intEnc(uint64(p.srcIface)),     /* src_iface */
				intEnc(uint64(p.tunnelIP4Dst)), /* tunnel_ipv4_dst */
				intEnc(uint64(p.tunnelTEID)),   /* enb_teid */
				intEnc(uint64(p.srcIP)),        /* ueaddr ip*/
				intEnc(uint64(p.dstIP)),        /* inet ip */
				intEnc(uint64(p.srcPort)),      /* ue port */
				intEnc(uint64(p.dstPort)),      /* inet port */
				intEnc(uint64(p.proto)),        /* proto id */
			},
			Masks: []*pb.FieldData{
				intEnc(uint64(p.srcIfaceMask)),     /* src_iface-mask */
				intEnc(uint64(p.tunnelIP4DstMask)), /* tunnel_ipv4_dst-mask */
				intEnc(uint64(p.tunnelTEIDMask)),   /* enb_teid-mask */
				intEnc(uint64(p.srcIPMask)),        /* ueaddr ip-mask */
				intEnc(uint64(p.dstIPMask)),        /* inet ip-mask */
				intEnc(uint64(p.srcPortMask)),      /* ue port-mask */
				intEnc(uint64(p.dstPortMask)),      /* inet port-mask */
				intEnc(uint64(p.protoMask)),        /* proto id-mask */
			},
		}

		any, err = anypb.New(f)
		if err != nil {
			log.Errorln("Error marshalling the rule", f, err)
			return
		}

		b.processPDR(ctx, any, upfMsgTypeDel)
		done <- true
	}()
}

func (b *bess) addQER(ctx context.Context, done chan<- bool, qer qer) {
	go func() {
		var (
			cir, pir, cbs, ebs, pbs, gate uint64
			srcIface                      uint8
		)

		// Uplink QER
		srcIface = access

		// Lookup QCI from QFI, else try default QCI.
		qosVal, ok := b.qciQosMap[qer.qfi]
		if !ok {
			log.Debug("No config for qfi/qci : ", qer.qfi, ". Using default burst size.")

			qosVal = b.qciQosMap[0]
		}

		cbs = maxUint64(calcBurstSizeFromRate(qer.ulGbr, uint64(qosVal.burstDurationMs)), uint64(qosVal.cbs))
		ebs = maxUint64(calcBurstSizeFromRate(qer.ulMbr, uint64(qosVal.burstDurationMs)), uint64(qosVal.ebs))
		pbs = maxUint64(calcBurstSizeFromRate(qer.ulMbr, uint64(qosVal.burstDurationMs)), uint64(qosVal.ebs))

		if qer.ulStatus == ie.GateStatusClosed {
			gate = qerGateStatusDrop
		} else if qer.ulMbr != 0 || qer.ulGbr != 0 {
			/* MBR/GBR is received in Kilobits/sec.
			   CIR/PIR is sent in bytes */
			cir = maxUint64(((qer.ulGbr * 1000) / 8), 1)
			pir = maxUint64(((qer.ulMbr * 1000) / 8), cir)
			gate = qerGateMeter
		} else {
			gate = qerGateUnmeter
		}

		if qer.qosLevel == ApplicationQos {
			b.addApplicationQER(ctx, gate, srcIface, cir, pir, cbs, pbs, ebs, qer)
		} else if qer.qosLevel == SessionQos {
			b.addSessionQER(ctx, gate, srcIface, cir, pir, cbs, pbs, ebs, qer)
		}

		// Downlink QER
		srcIface = core

		// Lookup QCI from QFI, else try default QCI.
		qosVal, ok = b.qciQosMap[qer.qfi]
		if !ok {
			log.Debug("No config for qfi/qci : ", qer.qfi, ". Using default burst size.")

			qosVal = b.qciQosMap[0]
		}

		cbs = maxUint64(calcBurstSizeFromRate(qer.dlGbr, uint64(qosVal.burstDurationMs)), uint64(qosVal.cbs))
		ebs = maxUint64(calcBurstSizeFromRate(qer.dlMbr, uint64(qosVal.burstDurationMs)), uint64(qosVal.ebs))
		pbs = maxUint64(calcBurstSizeFromRate(qer.dlMbr, uint64(qosVal.burstDurationMs)), uint64(qosVal.ebs))

		if qer.dlStatus == ie.GateStatusClosed {
			gate = qerGateStatusDrop
		} else if qer.dlMbr != 0 || qer.dlGbr != 0 {
			/* MBR/GBR is received in Kilobits/sec.
			   CIR/PIR is sent in bytes */
			cir = maxUint64(((qer.dlGbr * 1000) / 8), 1)
			pir = maxUint64(((qer.dlMbr * 1000) / 8), cir)
			gate = qerGateMeter
		} else {
			gate = qerGateUnmeter
		}

		if qer.qosLevel == ApplicationQos {
			b.addApplicationQER(ctx, gate, srcIface, cir, pir, cbs, pbs, ebs, qer)
		} else if qer.qosLevel == SessionQos {
			b.addSessionQER(ctx, gate, srcIface, cir, pir, cbs, pbs, ebs, qer)
		}

		done <- true
	}()
}

func (b *bess) addApplicationQER(ctx context.Context, gate uint64, srcIface uint8,
	cir uint64, pir uint64, cbs uint64, pbs uint64,
	ebs uint64, qer qer) {
	var (
		any *anypb.Any
		err error
	)

	q := &pb.QosCommandAddArg{
		Gate: gate,
		Cir:  cir, /* committed info rate */
		Pir:  pir, /* peak info rate */
		Cbs:  cbs, /* committed burst size */
		Pbs:  pbs, /* Peak burst size */
		Ebs:  ebs, /* Excess burst size */
		Fields: []*pb.FieldData{
			intEnc(uint64(srcIface)),  /* Src Intf */
			intEnc(uint64(qer.qerID)), /* qer_id */
			intEnc(qer.fseID),         /* fseid */
		},
		Values: []*pb.FieldData{
			intEnc(uint64(qer.qfi)), /* QFI */
		},
	}

	any, err = anypb.New(q)
	if err != nil {
		log.Errorln("Error marshalling the rule", q, err)
		return
	}

	qosTableName := AppQerLookup

	err = b.processQER(ctx, any, upfMsgTypeAdd, qosTableName)
	if err != nil {
		log.Errorln("process QER failed for appQERLookup add operation")
	}
}

func (b *bess) delQER(ctx context.Context, done chan<- bool, qer qer) {
	go func() {
		var srcIface uint8

		// Uplink QER
		srcIface = access

		if qer.qosLevel == ApplicationQos {
			b.delApplicationQER(ctx, srcIface, qer)
		} else if qer.qosLevel == SessionQos {
			b.delSessionQER(ctx, srcIface, qer)
		}

		// Downlink QER
		srcIface = core

		if qer.qosLevel == ApplicationQos {
			b.delApplicationQER(ctx, srcIface, qer)
		} else if qer.qosLevel == SessionQos {
			b.delSessionQER(ctx, srcIface, qer)
		}

		done <- true
	}()
}

func (b *bess) delApplicationQER(
	ctx context.Context, srcIface uint8, qer qer) {
	var (
		any *anypb.Any
		err error
	)

	q := &pb.QosCommandDeleteArg{
		Fields: []*pb.FieldData{
			intEnc(uint64(srcIface)),  /* Src Intf */
			intEnc(uint64(qer.qerID)), /* qer_id */
			intEnc(qer.fseID),         /* fseid */
		},
	}

	any, err = anypb.New(q)
	if err != nil {
		log.Println("Error marshalling the rule", q, err)
		return
	}

	qosTableName := AppQerLookup

	err = b.processQER(ctx, any, upfMsgTypeDel, qosTableName)
	if err != nil {
		log.Errorln("process QER failed for appQERLookup del operation")
	}
}

func (b *bess) processFAR(ctx context.Context, any *anypb.Any, method upfMsgType) {
	if method != upfMsgTypeAdd && method != upfMsgTypeDel && method != upfMsgTypeClear {
		log.Println("Invalid method name: ", method)
		return
	}

	methods := [...]string{"add", "add", "delete", "clear"}

	resp, err := b.client.ModuleCommand(ctx, &pb.CommandRequest{
		Name: "farLookup",
		Cmd:  methods[method],
		Arg:  any,
	})

	log.Traceln("farlookup resp : ", resp)

	if err != nil || resp.GetError() != nil {
		log.Errorf("farLookup method failed with resp: %v, err: %v\n", resp, err)
	}
}

func (b *bess) setActionValue(f far) uint8 {
	if (f.applyAction & ActionForward) != 0 {
		if f.dstIntf == ie.DstInterfaceAccess {
			return farForwardD
		} else if (f.dstIntf == ie.DstInterfaceCore) || (f.dstIntf == ie.DstInterfaceSGiLANN6LAN) {
			return farForwardU
		}
	} else if (f.applyAction & ActionDrop) != 0 {
		return farDrop
	} else if (f.applyAction & ActionBuffer) != 0 {
		return farNotify
	} else if (f.applyAction & ActionNotify) != 0 {
		return farNotify
	}

	// default action
	return farDrop
}

func (b *bess) addFAR(ctx context.Context, done chan<- bool, far far) {
	go func() {
		var (
			any *anypb.Any
			err error
		)

		action := b.setActionValue(far)
		f := &pb.ExactMatchCommandAddArg{
			Gate: uint64(far.tunnelType),
			Fields: []*pb.FieldData{
				intEnc(uint64(far.farID)), /* far_id */
				intEnc(far.fseID),         /* fseid */
			},
			Values: []*pb.FieldData{
				intEnc(uint64(action)),           /* action */
				intEnc(uint64(far.tunnelType)),   /* tunnel_out_type */
				intEnc(uint64(far.tunnelIP4Src)), /* access-ip */
				intEnc(uint64(far.tunnelIP4Dst)), /* enb ip */
				intEnc(uint64(far.tunnelTEID)),   /* enb teid */
				intEnc(uint64(far.tunnelPort)),   /* udp gtpu port */
			},
		}

		any, err = anypb.New(f)
		if err != nil {
			log.Println("Error marshalling the rule", f, err)
			return
		}

		b.processFAR(ctx, any, upfMsgTypeAdd)
		done <- true
	}()
}

func (b *bess) delFAR(ctx context.Context, done chan<- bool, far far) {
	go func() {
		var (
			any *anypb.Any
			err error
		)

		f := &pb.ExactMatchCommandDeleteArg{
			Fields: []*pb.FieldData{
				intEnc(uint64(far.farID)), /* far_id */
				intEnc(far.fseID),         /* fseid */
			},
		}

		any, err = anypb.New(f)
		if err != nil {
			log.Println("Error marshalling the rule", f, err)
			return
		}

		b.processFAR(ctx, any, upfMsgTypeDel)
		done <- true
	}()
}

func (b *bess) processSliceMeter(ctx context.Context, any *anypb.Any, method upfMsgType) {
	if method != upfMsgTypeAdd && method != upfMsgTypeDel && method != upfMsgTypeClear {
		log.Errorln("Invalid method name: ", method)
		return
	}

	methods := [...]string{"add", "add", "delete", "clear"}

	_, err := b.client.ModuleCommand(
		ctx, &pb.CommandRequest{
			Name: "sliceMeter",
			Cmd:  methods[method],
			Arg:  any,
		},
	)
	if err != nil {
		log.Errorln("sliceMeter method failed!:", err)
	}
}

func (b *bess) addSliceMeter(ctx context.Context, done chan<- bool, meterConfig SliceMeterConfig) {
	go func() {
		var (
			any                           *anypb.Any
			err                           error
			cir, pir, cbs, ebs, pbs, gate uint64
		)

		// Uplink N6 slice meter config
		if meterConfig.N6RateBps != 0 {
			gate = sliceMeterGateMeter
			cir = 1                         // Mark all traffic as yellow
			pir = meterConfig.N6RateBps / 8 // bit/s to byte/s
		} else {
			gate = sliceMeterGateUnmeter
		}

		if meterConfig.N6BurstBytes != 0 {
			cbs = 1 // Mark all traffic as yellow
			pbs = meterConfig.N6BurstBytes
			ebs = 0 // Unused
		} else {
			cbs = 1 // Mark all traffic as yellow
			pbs = DefaultBurstSize
			ebs = 0 // Unused
		}

		log.Traceln("uplink slice : cir: ", cir, ", pir: ", pir,
			", cbs: ", cbs, ", pbs: ", pbs)

		q := &pb.QosCommandAddArg{
			Gate:              gate,
			Cir:               cir,                               /* committed info rate */
			Pir:               pir,                               /* peak info rate */
			Cbs:               cbs,                               /* committed burst size */
			Pbs:               pbs,                               /* Peak burst size */
			Ebs:               ebs,                               /* Excess burst size */
			OptionalDeductLen: &pb.QosCommandAddArg_DeductLen{0}, /* Include all headers */
			Fields: []*pb.FieldData{
				intEnc(uint64(farForwardU)), /* Action */
				intEnc(uint64(0)),           /* tunnel_out_type */
			},
		}

		any, err = anypb.New(q)
		if err != nil {
			log.Errorln("Error marshalling the rule", q, err)
			return
		}

		b.processSliceMeter(ctx, any, upfMsgTypeAdd)

		// Downlink N3 slice meter config
		if meterConfig.N3RateBps != 0 {
			gate = sliceMeterGateMeter
			cir = 1                         // Mark all traffic as yellow
			pir = meterConfig.N3RateBps / 8 // bit/s to byte/s
		} else {
			gate = sliceMeterGateUnmeter
		}

		if meterConfig.N3BurstBytes != 0 {
			cbs = 1 // Mark all traffic as yellow
			pbs = meterConfig.N3BurstBytes
			ebs = 0 // Unused
		} else {
			cbs = 1 // Mark all traffic as yellow
			pbs = DefaultBurstSize
			ebs = 0 // Unused
		}

		log.Traceln("downlink slice : cir: ", cir, ", pir: ", pir,
			", cbs: ", cbs, ", pbs: ", pbs)
		// TODO: packet deduction should take GTPU extension header into account
		q = &pb.QosCommandAddArg{
			Gate:              gate,
			Cir:               cir,                                /* committed info rate */
			Pir:               pir,                                /* peak info rate */
			Cbs:               cbs,                                /* committed burst size */
			Pbs:               pbs,                                /* Peak burst size */
			Ebs:               ebs,                                /* Excess burst size */
			OptionalDeductLen: &pb.QosCommandAddArg_DeductLen{50}, /* Exclude Ethernet,IP,UDP,GTP header */
			Fields: []*pb.FieldData{
				intEnc(uint64(farForwardD)), /* Action */
				intEnc(uint64(1)),           /* tunnel_out_type */
			},
		}

		any, err = anypb.New(q)
		if err != nil {
			log.Errorln("Error marshalling the rule", q, err)
			return
		}

		b.processSliceMeter(ctx, any, upfMsgTypeAdd)
		done <- true
	}()
}

func (b *bess) processQER(ctx context.Context, any *anypb.Any, method upfMsgType, qosTableName string) error {
	if method != upfMsgTypeAdd && method != upfMsgTypeDel && method != upfMsgTypeClear {
		return fmt.Errorf("invalid method name: %v", method)
	}

	methods := [...]string{"add", "add", "delete", "clear"}

	resp, err := b.client.ModuleCommand(ctx, &pb.CommandRequest{
		Name: qosTableName,
		Cmd:  methods[method],
		Arg:  any,
	})

	log.Traceln("qerlookup resp : ", resp)

	if err != nil || resp.GetError() != nil {
		log.Errorf("%v for qer %v failed with resp: %v, error: %v", qosTableName, methods[method], resp, err)
		return err
	}

	return nil
}

func (b *bess) addSessionQER(ctx context.Context, gate uint64, srcIface uint8,
	cir uint64, pir uint64, cbs uint64,
	pbs uint64, ebs uint64, qer qer) {
	var (
		any *anypb.Any
		err error
	)

	q := &pb.QosCommandAddArg{
		Gate: gate,
		Cir:  cir, /* committed info rate */
		Pir:  pir, /* peak info rate */
		Cbs:  cbs, /* committed burst size */
		Pbs:  pbs, /* Peak burst size */
		Ebs:  ebs, /* Excess burst size */
		Fields: []*pb.FieldData{
			intEnc(uint64(srcIface)), /* Src Intf */
			intEnc(qer.fseID),        /* fseid */
		},
	}

	any, err = anypb.New(q)
	if err != nil {
		log.Errorln("Error marshalling the rule", q, err)
		return
	}

	qosTableName := SessQerLookup

	err = b.processQER(ctx, any, upfMsgTypeAdd, qosTableName)
	if err != nil {
		log.Errorln("process QER failed for sessionQERLookup add operation")
	}
}

func (b *bess) delSessionQER(ctx context.Context, srcIface uint8, qer qer) {
	var (
		any *anypb.Any
		err error
	)

	q := &pb.QosCommandDeleteArg{
		Fields: []*pb.FieldData{
			intEnc(uint64(srcIface)), /* Src Intf */
			intEnc(qer.fseID),        /* fseid */
		},
	}

	any, err = anypb.New(q)
	if err != nil {
		log.Println("Error marshalling the rule", q, err)
		return
	}

	qosTableName := SessQerLookup

	err = b.processQER(ctx, any, upfMsgTypeDel, qosTableName)
	if err != nil {
		log.Errorln("process QER failed for sessionQERLookup del operation")
	}
}

func (b *bess) GRPCJoin(calls int, timeout time.Duration, done chan bool) bool {
	boom := time.After(timeout)

	for {
		select {
		case ok := <-done:
			if !ok {
				log.Println("Error making GRPC calls")
				return false
			}

			calls--
			if calls == 0 {
				return true
			}
		case <-boom:
			log.Println("Timed out adding entries")
			return false
		}
	}
}
