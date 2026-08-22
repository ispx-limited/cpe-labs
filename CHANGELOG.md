# Changelog

## [0.7.0](https://github.com/ispx-limited/cpe-labs/compare/v0.6.0...v0.7.0) (2026-08-22)


### Added

* **release:** the changelog is generated and published on the docs site ([#37](https://github.com/ispx-limited/cpe-labs/issues/37)) ([d4f110d](https://github.com/ispx-limited/cpe-labs/commit/d4f110dd23126d72cab1d0c6e20cb2149553775e))
* **softwaremodules:** TR-157 software modules with an app that extends the data model ([#49](https://github.com/ispx-limited/cpe-labs/issues/49)) ([70c9cbd](https://github.com/ispx-limited/cpe-labs/commit/70c9cbdfa972e6fd2f16275cff660f37611ff3b6))
* **usp:** an agent can lose its uplink without telling the broker ([#51](https://github.com/ispx-limited/cpe-labs/issues/51)) ([ca3913d](https://github.com/ispx-limited/cpe-labs/commit/ca3913d350df428610e9ae9b0cf061803062a50e))
* **usp:** bulk data profiles push reports as the Push! event ([#50](https://github.com/ispx-limited/cpe-labs/issues/50)) ([ec36670](https://github.com/ispx-limited/cpe-labs/commit/ec366709dbf29da51ff5d5a12afa4eb4b032d6a4))


### Fixed

* **profiles:** the boot Inform after a firmware Download carries the new version ([#48](https://github.com/ispx-limited/cpe-labs/issues/48)) ([0e22d5f](https://github.com/ispx-limited/cpe-labs/commit/0e22d5f37329940650079ccc7ed8d748d555f816))
* **profiles:** the Calix link fault lasts long enough to be seen ([#52](https://github.com/ispx-limited/cpe-labs/issues/52)) ([0cd9252](https://github.com/ispx-limited/cpe-labs/commit/0cd92527547dff76ae54f95808bb8ed27e9bfcba))

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
