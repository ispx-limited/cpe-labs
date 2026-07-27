---
hide:
  - toc
---

<section class="cpelabs-home" markdown>

<div id="hero-tree" class="hero-stage" aria-hidden="true">
  <div class="hero-stage-inner"></div>
</div>

<div class="cpelabs-home__copy" markdown>

<h1 class="cpelabs-sr-only">cpe-labs</h1>

<p class="cpelabs-kicker">A CPE simulator that bridges the developer gap for CI/CD against TR-069 and TR-369 devices.</p>

<div class="cpelabs-actions" markdown>
[Get started](guides/quickstart.md){ .cpelabs-btn .cpelabs-btn--primary }
[See the architecture](overview/architecture.md){ .cpelabs-btn .cpelabs-btn--ghost }
</div>

<div class="cpelabs-signal__chips">
  <span class="cpelabs-chip">TR-069 CWMP</span>
  <span class="cpelabs-chip">TR-369 USP</span>
  <span class="cpelabs-chip">Multi-CPE</span>
  <span class="cpelabs-chip">Vendor profiles</span>
</div>

</div>
</section>

## Built for ACS / Controller integration testing

<div class="cpelabs-card-grid" markdown>

<div class="cpelabs-card" markdown>
### Real wire format
Real TR-069 SOAP and TR-369 USP on the wire. The ACS or Controller sees a real agent, not a mock.
</div>

<div class="cpelabs-card" markdown>
### Profile-driven behavior
Vendor profiles are YAML. Parameter tree, generators, fleet, periodic cadence, connection-request auth, all in one file. New vendor? Drop in a profile, no recompile.
</div>

<div class="cpelabs-card" markdown>
### Multi-CPE per process
One binary spawns N independent simulated CPEs across both protocol stacks, sharing one parameter tree per CPE.
</div>

<div class="cpelabs-card" markdown>
### CI-friendly
Deterministic via `--seed=N`. Daemon mode runs in compose alongside an ACS or Controller. Every log line carries `cpe_id`.
</div>

</div>
