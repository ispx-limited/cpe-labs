#!/bin/bash
# Runtime bring-up for the icwmp CPE simulator. Order matters: ubusd is
# the bus everything else attaches to, rpcd exposes uci over ubus,
# bbfdmd/dm-service serve the TR-181 tree, icwmpd is the CWMP client.
set -x

mkdir -p /var/run /var/state/icwmp

[ -n "$ACS_URL" ] && uci set cwmp.acs.url="$ACS_URL"
[ -n "$ACS_USERNAME" ] && uci set cwmp.acs.userid="$ACS_USERNAME"
[ -n "$ACS_PASSWORD" ] && uci set cwmp.acs.passwd="$ACS_PASSWORD"
uci set cwmp.acs.periodic_inform_enable='1'
uci set cwmp.acs.periodic_inform_interval="${INFORM_INTERVAL:-60}"
uci commit cwmp

# Device identity, so one image can simulate any vendor tuple.
BOARD="uci -c /etc/board-db/config"
[ -n "$MANUFACTURER" ] && $BOARD set device.deviceinfo.Manufacturer="$MANUFACTURER"
[ -n "$OUI" ] && $BOARD set device.deviceinfo.ManufacturerOUI="$OUI"
[ -n "$SERIAL" ] && $BOARD set device.deviceinfo.SerialNumber="$SERIAL"
[ -n "$PRODUCT_CLASS" ] && $BOARD set device.deviceinfo.ProductClass="$PRODUCT_CLASS"
[ -n "$MODEL_NAME" ] && $BOARD set device.deviceinfo.ModelName="$MODEL_NAME"
[ -n "$SOFTWARE_VERSION" ] && $BOARD set device.deviceinfo.SoftwareVersion="$SOFTWARE_VERSION"
$BOARD commit device

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

exec icwmpd
