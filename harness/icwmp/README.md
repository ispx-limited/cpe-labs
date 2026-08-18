# icwmp harness

A real TR-069 client in a container: the iopsys `icwmpd` with the full
bbfdm data-model stack, pointed at your ACS. Where cpe-sim replays a
declared parameter tree at fleet scale, this serves one device's tree
live from real daemons, and is the thing to reach for when you want to
know how your ACS behaves against a client you did not model yourself.

```bash
docker build -t icwmp-harness harness/icwmp/

docker run -d --name icwmp --hostname icwmp \
  -e ACS_URL=http://your-acs:7547/ \
  -e MANUFACTURER=OpenWrt -e OUI=021024 -e SERIAL=OWRT000001 \
  -e PRODUCT_CLASS=DockerCPE -e MODEL_NAME=icwmp-harness \
  icwmp-harness
```

The full guide, including the identity env vars, the limitations and
the build traps, is at
[docs/guides/icwmp.md](../../docs/guides/icwmp.md).
