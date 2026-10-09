# icwmp harness: the iopsys TR-069 client (icwmp) plus the bbfdm
# data-model stack, built on the iopsys CI image, which ships ubusd,
# rpcd, uci and the toolchain preinstalled. This is the OpenWRT software
# stack without an OpenWRT rootfs; the data model is served live by
# bbfdm daemons, not replayed from a capture, so GPV/SPV hit real code
# paths and unknown parameters fault the way lab hardware faults.
#
# The base image is ~4GB and its ENTRYPOINT launches supervisord and an
# interactive bash, ignoring any command passed to docker run. Debug
# with --entrypoint /bin/bash or docker exec, never docker run <cmd>.
FROM dev.iopsys.eu:5050/iopsys/gitlab-ci-pipeline/code-analysis:1.2

# Pinned because devel is a moving branch and this build chain (bbfdm,
# micro-service managers, plugin ABI) drifts together.
ARG ICWMP_COMMIT=c95bf4de3a9885811ceb6cb44d50403ca27493b5

RUN git clone https://dev.iopsys.eu/bbf/icwmp.git /opt/dev/icwmp && \
    cd /opt/dev/icwmp && git checkout ${ICWMP_COMMIT}
WORKDIR /opt/dev/icwmp

# Clones and builds bbfdmd, dm-service, libbbfdm and the micro-service
# managers (sysmngr, ethmngr, wifimngr, netmngr). Its final step starts
# services under supervisord, which cannot run in a build layer, so the
# script exits 1 after a successful install; the artifact test on the
# next line is the real pass/fail.
RUN ./gitlab-ci/install-dependencies.sh || true
RUN test -x /usr/sbin/bbfdmd && test -x /usr/sbin/dm-service && test -x /usr/sbin/sysmngr

# icwmpd and its bbfdm plugin. make install expects the binaries copied
# back into the source tree (out-of-tree build quirk in their cmake),
# which is the same cp dance their CI's build_icwmp() does.
RUN mkdir -p build && cd build && \
    cmake .. -DCMAKE_INSTALL_PREFIX=/ -DBBF_VENDOR_PREFIX="X_IOWRT_EU_" \
      -DCMAKE_C_FLAGS="-DICWMP_ENABLE_VENDOR_EXTN -DICWMP_ENABLE_SMM_SUPPORT -DICWMP_ENABLE_OPCONF_SUPPORT" && \
    make && \
    cp src/icwmpd ../src/ && cp bbf_plugin/libcwmpdm.so ../bbf_plugin/ && \
    make install && \
    ln -sf /opt/dev/icwmp/build/bbf_plugin/libcwmpdm.so /usr/share/bbfdm/micro_services/icwmp.so && \
    test -x /usr/sbin/icwmpd

# stunc, the iopsys TR-069 Annex G client: binding discovery against the
# ACS's STUN server, keepalives, and the listener that validates the
# UDP Connection Requests the ACS sends back through the binding and
# tells icwmpd to inform over ubus. Its bbfdm plugin adds the
# Device.ManagementServer.STUN* leaves and is installed as an extension
# of the icwmp micro-service (the directory named after the service,
# the way netmngr carries libtr143d.so), not as a service of its own:
# two services both claiming ManagementServer leave every leaf of it
# faulting 9005, ConnectionRequestURL included, and icwmpd never
# informs. icwmp's own plugin serves UDPConnectionRequestAddress and
# NATDetected from the state stunc writes. Pinned for the same reason
# icwmp is. Built with -funsigned-char because stunc prints its
# HMAC-SHA1 with "%02X" from a plain char: unsigned on the ARM routers
# it ships on, signed on x86, where every digest byte above 0x7F
# became FFFFFFxx and every UDP connection request failed its
# signature check.
ARG STUNC_COMMIT=741ef6aa2f07e77bb4e5d12f3c3bc6cad8d04a61
RUN git clone https://dev.iopsys.eu/bbf/stunc.git /opt/dev/stunc && \
    cd /opt/dev/stunc && git checkout ${STUNC_COMMIT} && \
    cd src && make CFLAGS=-funsigned-char && \
    install -m 755 stunc /usr/sbin/stunc && \
    mkdir -p /usr/share/bbfdm/micro_services/icwmp && \
    install -m 755 libstunc.so /usr/share/bbfdm/micro_services/icwmp/libstunc.so && \
    cp /opt/dev/stunc/test/files/etc/config/stunc /etc/config/stunc && \
    test -x /usr/sbin/stunc

# uci config, board-db identity and the dummy sysupgrade from the CI
# fixture tree. start.sh overrides identity per container from env.
RUN cp -rf test/files/* / && cp test/dummy/sysupgrade /sbin/ && chmod 777 /sbin/sysupgrade && \
    mkdir -p /usr/share/rpcd/schemas && cp -r schemas/ubus/*.json /usr/share/rpcd/schemas/ && \
    mkdir -p /tmp/firmware


COPY start.sh /start.sh
RUN chmod +x /start.sh
ENTRYPOINT []
CMD ["/start.sh"]
