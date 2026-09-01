# Changelog

## [0.8.0](https://github.com/ispx-limited/cpe-labs/compare/v0.7.0...v0.8.0) (2026-09-01)


### Added

* **profiles:** a Sagemcom FAST5599 whose uplink is the last port and whose accounts are not Device.Users ([#57](https://github.com/ispx-limited/cpe-labs/issues/57)) ([4bc650a](https://github.com/ispx-limited/cpe-labs/commit/4bc650a70495dacf36ad78ac23ebddda23425c0d))
* **profiles:** a TR-098 gateway that supports bulk data collection ([#58](https://github.com/ispx-limited/cpe-labs/issues/58)) ([43f8716](https://github.com/ispx-limited/cpe-labs/commit/43f871649230d5456d4d7250c121a9dd151b7da7))
* **profiles:** an ASUS RT-AX88U that keeps its mesh, site survey and remote access under vendor trees ([#56](https://github.com/ispx-limited/cpe-labs/issues/56)) ([8a04ff3](https://github.com/ispx-limited/cpe-labs/commit/8a04ff374813bcf607ebaf56bb1bd772edd231e1))


### Fixed

* **profiles:** every example declares where its ACS credentials live ([#60](https://github.com/ispx-limited/cpe-labs/issues/60)) ([d35d347](https://github.com/ispx-limited/cpe-labs/commit/d35d347c9ba36c8cc09623d8ffcdc39609ba393f))

## [0.7.0](https://github.com/ispx-limited/cpe-labs/compare/v0.6.0...v0.7.0) (2026-08-24)


### Added

* **profiles:** the NVG and FAST5598 expose their local account tables ([#54](https://github.com/ispx-limited/cpe-labs/issues/54)) ([fdd827d](https://github.com/ispx-limited/cpe-labs/commit/fdd827dc7e947602f724234689a07adf87f354f4))
* **release:** the changelog is generated and published on the docs site ([#37](https://github.com/ispx-limited/cpe-labs/issues/37)) ([d4f110d](https://github.com/ispx-limited/cpe-labs/commit/d4f110dd23126d72cab1d0c6e20cb2149553775e))
* **softwaremodules:** TR-157 software modules with an app that extends the data model ([#49](https://github.com/ispx-limited/cpe-labs/issues/49)) ([70c9cbd](https://github.com/ispx-limited/cpe-labs/commit/70c9cbdfa972e6fd2f16275cff660f37611ff3b6))
* **usp:** an agent can lose its uplink without telling the broker ([#51](https://github.com/ispx-limited/cpe-labs/issues/51)) ([ca3913d](https://github.com/ispx-limited/cpe-labs/commit/ca3913d350df428610e9ae9b0cf061803062a50e))
* **usp:** bulk data profiles push reports as the Push! event ([#50](https://github.com/ispx-limited/cpe-labs/issues/50)) ([ec36670](https://github.com/ispx-limited/cpe-labs/commit/ec366709dbf29da51ff5d5a12afa4eb4b032d6a4))


### Fixed

* **profiles:** the boot Inform after a firmware Download carries the new version ([#48](https://github.com/ispx-limited/cpe-labs/issues/48)) ([0e22d5f](https://github.com/ispx-limited/cpe-labs/commit/0e22d5f37329940650079ccc7ed8d748d555f816))
* **profiles:** the Calix link fault lasts long enough to be seen ([#52](https://github.com/ispx-limited/cpe-labs/issues/52)) ([0cd9252](https://github.com/ispx-limited/cpe-labs/commit/0cd92527547dff76ae54f95808bb8ed27e9bfcba))
* **profiles:** the Calix link fault takes fifty agents dark, not the process ([#53](https://github.com/ispx-limited/cpe-labs/issues/53)) ([bb8acea](https://github.com/ispx-limited/cpe-labs/commit/bb8aceaf7bf4d10f1663f298b262049c29193fa1))

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
