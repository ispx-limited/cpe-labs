#!/bin/bash
# Runtime bring-up for the icwmp CPE simulator. Order matters: ubusd is
# the bus everything else attaches to, rpcd exposes uci over ubus,
# bbfdmd/dm-service serve the TR-181 tree, icwmpd is the CWMP client.
set -x

mkdir -p /var/run /var/state/icwmp

[ -n "$ACS_URL" ] && uci set cwmp.acs.url="$ACS_URL"
[ -n "$ACS_USERNAME" ] && uci set cwmp.acs.userid="$ACS_USERNAME"
# Set-but-empty is a password too: an ACS whose factory rule expects an
# empty one cannot be reached by a client that keeps the fixture's.
[ -n "${ACS_PASSWORD+x}" ] && uci set cwmp.acs.passwd="$ACS_PASSWORD"
uci set cwmp.acs.periodic_inform_enable='1'
uci set cwmp.acs.periodic_inform_interval="${INFORM_INTERVAL:-60}"
# The connection request credential the client challenges with over
# HTTP and validates UDP Connection Requests against. stunc reads the
# password from cwmp.cpe.password where icwmp keeps it in passwd, so
# both are written.
if [ -n "$CR_USERNAME" ]; then
    uci set cwmp.cpe.userid="$CR_USERNAME"
    uci set cwmp.cpe.passwd="$CR_PASSWORD"
    uci set cwmp.cpe.password="$CR_PASSWORD"
fi
uci commit cwmp

# STUN (TR-069 Annex G). Enabled from env rather than left to the ACS
# because SetParameterValues does not persist in this container (see
# the guide's limitations); the ACS still reads the leaves back.
if [ -n "$STUN_SERVER" ]; then
    uci set stunc.stunc.enabled='1'
    uci set stunc.stunc.server_address="$STUN_SERVER"
    uci set stunc.stunc.server_port="${STUN_PORT:-3478}"
    uci set stunc.stunc.min_keepalive="${STUN_MIN_KEEPALIVE:-30}"
    uci set stunc.stunc.max_keepalive="${STUN_MAX_KEEPALIVE:-120}"
    uci set stunc.stunc.client_port="${STUN_CLIENT_PORT:-7547}"
    uci set stunc.stunc.log_level='4'
else
    uci set stunc.stunc.enabled='0'
fi
uci commit stunc

# Behind a NAT container: route everything through it.
[ -n "$GATEWAY" ] && ip route replace default via "$GATEWAY"

# Device identity, so one image can simulate any vendor tuple.
BOARD="uci -c /etc/board-db/config"
[ -n "$MANUFACTURER" ] && $BOARD set device.deviceinfo.Manufacturer="$MANUFACTURER"
[ -n "$OUI" ] && $BOARD set device.deviceinfo.ManufacturerOUI="$OUI"
[ -n "$SERIAL" ] && $BOARD set device.deviceinfo.SerialNumber="$SERIAL"
[ -n "$PRODUCT_CLASS" ] && $BOARD set device.deviceinfo.ProductClass="$PRODUCT_CLASS"
[ -n "$MODEL_NAME" ] && $BOARD set device.deviceinfo.ModelName="$MODEL_NAME"
[ -n "$SOFTWARE_VERSION" ] && $BOARD set device.deviceinfo.SoftwareVersion="$SOFTWARE_VERSION"
$BOARD commit device

# No syslog daemon runs here and stunc logs through syslog(3) alone, so
# a sink on /dev/log keeps its lines, and bbfdm's, in /var/log/syslog.
python3 -c '
import os, socket
try:
    os.unlink("/dev/log")
except FileNotFoundError:
    pass
s = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
s.bind("/dev/log")
os.chmod("/dev/log", 0o666)
with open("/var/log/syslog", "ab", buffering=0) as f:
    while True:
        f.write(s.recv(8192) + b"\n")
' &

ubusd &
sleep 1
rpcd &
sleep 1

# Core data-model daemon, then the data-model providers. Plugin-style
# services (a .so under micro_services/) run via dm-service; the rest
# (sysmngr, ethmngr, wifimngr) are standalone daemons in /usr/sbin that
# register their subtree themselves. sysmngr serves Device.DeviceInfo.,
# without which icwmpd cannot build an Inform.
bbfdmd -l 7 &
sleep 1
for svc in /etc/bbfdm/services/*.json; do
    [ -f "$svc" ] || continue
    name=$(basename "$svc" .json)
    if [ -e "/usr/share/bbfdm/micro_services/${name}.so" ]; then
        dm-service -m "$name" &
    elif [ -x "/usr/sbin/${name}" ]; then
        "/usr/sbin/${name}" &
    fi
done
sleep 3

# stunc reads its uci once at start and has no reload, and there is no
# procd here to restart it on a config change or a crash, so a loop
# does both: it restarts stunc whenever /etc/config/stunc changes and
# keeps one running while enabled. icwmpd must be up first, since stunc
# announces a connection request over ubus to it.
stunc_supervise() {
    local last=""
    while true; do
        local sig
        sig=$(md5sum /etc/config/stunc | cut -d' ' -f1)
        if [ "$sig" != "$last" ]; then
            last=$sig
            pkill -x stunc
        fi
        if [ "$(uci -q get stunc.stunc.enabled)" = "1" ] && ! pgrep -x stunc >/dev/null; then
            stunc &
        fi
        sleep 2
    done
}
(sleep 5; stunc_supervise) &

exec icwmpd
