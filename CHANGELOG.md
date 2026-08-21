# Changelog

## 0.6.0 (2026-08-20)

### Added

* Triggered diagnostics on the simulated CPEs, including a neighbour scan, on
  both data model families.
* NAT port-mapping tables that start empty and count what the ACS adds.
* TR-104 voice on the ARRIS profile, with scripted call activity.
* Device-internal writes honor the notification attributes set by the ACS.
* icwmp, a real TR-069 client, runs in a container alongside the simulators.
* Sagemcom F@ST 5280 and ARRIS NVG599 reference gateway profiles.

### Fixed

* The ARRIS voice pool is sized for the whole fleet.
* Echo timestamps expand as the profile declares.
