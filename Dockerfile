# SPDX-License-Identifier: Apache-2.0
# Copyright 2020-present Open Networking Foundation
# Copyright (c) 2019 Intel Corporation

# Multi-stage Dockerfile
# Stage bess-build: builds bess with its dependencies
FROM ghcr.io/omec-project/upf-epc/bess_build AS bess-build
ARG CPU=native
RUN apt-get update && \
    apt-get -y install --no-install-recommends \
        ca-certificates \
        libelf-dev
        
ARG MAKEFLAGS

ENV PKG_CONFIG_PATH=/usr/lib64/pkgconfig

# linux ver should match target machine's kernel
WORKDIR /libbpf
ARG LIBBPF_VER=v0.3
RUN curl -L https://github.com/libbpf/libbpf/tarball/${LIBBPF_VER} | \
    tar xz -C . --strip-components=1 && \
    cp include/uapi/linux/if_xdp.h /usr/include/linux && \
    cd src && \
    make install && \
    ldconfig

# BESS pre-reqs
WORKDIR /bess
ARG BESS_COMMIT=dpdk-2011-focal
RUN curl -L https://github.com/NetSys/bess/tarball/${BESS_COMMIT} | \
    tar xz -C . --strip-components=1

# Patch BESS, patch and build DPDK
COPY patches/dpdk/* deps/
COPY patches/bess patches
RUN cat patches/* | patch -p1 && \
    ./build.py dpdk

# Plugins
RUN mkdir -p plugins

## SequentialUpdate
RUN mv sample_plugin plugins

## Network Token
ARG ENABLE_NTF
ARG NTF_COMMIT=master
COPY install_ntf.sh .
RUN ./install_ntf.sh

# Build and copy artifacts
COPY core/ core/
COPY build_bess.sh .
RUN ./build_bess.sh && \
    cp bin/bessd /bin && \
    mkdir -p /bin/modules && \
    cp core/modules/*.so /bin/modules && \
    mkdir -p /opt/bess && \
    cp -r bessctl pybess /opt/bess && \
    cp -r core/pb /pb && \
    cp -a protobuf /protobuf

# Stage bess: creates the runtime image of BESS
FROM python:3.9.7-slim AS bess
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        gcc \
        libgraph-easy-perl \
        iproute2 \
        iptables \
        iputils-ping \
        tcpdump && \
    rm -rf /var/lib/apt/lists/* && \
    pip install --no-cache-dir \
        flask \
        grpcio \
        iptools \
        mitogen \
        protobuf \
        psutil \
        pyroute2 \
        scapy && \
    apt-get --purge remove -y \
        gcc
COPY --from=bess-build /opt/bess /opt/bess
COPY --from=bess-build /bin/bessd /bin/bessd
COPY --from=bess-build /bin/modules /bin/modules
COPY conf /opt/bess/bessctl/conf
RUN ln -s /opt/bess/bessctl/bessctl /bin
ENV PYTHONPATH="/opt/bess"
WORKDIR /opt/bess/bessctl
ENTRYPOINT ["bessd", "-f"]

# Stage build bess golang pb
FROM golang AS protoc-gen
RUN go get github.com/golang/protobuf/protoc-gen-go

FROM bess-build AS go-pb
COPY --from=protoc-gen /go/bin/protoc-gen-go /bin
RUN mkdir /bess_pb && \
    protoc -I /usr/include -I /protobuf/ \
        /protobuf/*.proto /protobuf/ports/*.proto \
        --go_opt=paths=source_relative --go_out=plugins=grpc:/bess_pb

FROM golang AS pfcpiface-build
WORKDIR /pfcpiface
COPY pfcpiface .
RUN CGO_ENABLED=0 go build -mod=vendor -o /bin/pfcpiface

# Stage pfcpiface: runtime image of pfcpiface toward SMF/SPGW-C
FROM alpine AS pfcpiface
COPY conf /opt/bess/bessctl/conf
COPY conf/p4/bin/p4info.bin conf/p4/bin/p4info.txt conf/p4/bin/bmv2.json /bin/
COPY --from=pfcpiface-build /bin/pfcpiface /bin
ENTRYPOINT [ "/bin/pfcpiface" ]

# Stage pb: dummy stage for collecting protobufs
FROM scratch AS pb
COPY --from=bess-build /protobuf /protobuf
COPY --from=go-pb /bess_pb /bess_pb

# Stage binaries: dummy stage for collecting artifacts
FROM scratch AS artifacts
COPY --from=bess /bin/bessd /
COPY --from=pfcpiface /bin/pfcpiface /
COPY --from=bess-build /bess /bess
