/**
 * cpe-labs hero topology animation.
 *
 *   profile (root) -> cpe instances -> objects/groups -> parameters -> generators
 *
 * Mirrors the actual cpe-labs domain: one YAML profile defines a fleet of
 * simulated CPEs; each CPE has a parameter tree of objects/groups; each
 * leaf parameter may carry a generator (counter / drift / enum / uptime /
 * wallclock) that mutates its value over time.
 *
 * React-Flow-style: DOM nodes (cards with icons + labels) connected by SVG
 * smoothstep paths (orthogonal segments with rounded corners). A CSS-3D
 * camera flies through a chosen path each cycle, zooming on each node and
 * pulling back to overview.
 */
(function () {
  if (typeof window === "undefined") return;
  if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

  const CONFIG = [
    { branch: 1, y:   0, xSpan:    0, kind: "root"      },
    { branch: 3, y: 165, xSpan:  940, kind: "cpe"       },
    { branch: 2, y: 320, xSpan:  260, kind: "object"    },
    { branch: 2, y: 475, xSpan:  100, kind: "param"     },
    { branch: 2, y: 630, xSpan:   48, kind: "generator" },
  ];
  const LEAF_LEVEL = CONFIG.length - 1;

  const ICONS = {
    // profile, YAML doc with bracket marks
    root:
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<path d="M14 3 H7 C 6 3 5 4 5 5 V19 C 5 20 6 21 7 21 H17 C 18 21 19 20 19 19 V8 Z"/>' +
      '<path d="M14 3 V8 H19"/>' +
      '<path d="M9 13 L11 13 M13 13 L15 13 M9 16 L13 16"/>' +
      "</svg>",
    // cpe, router/gateway
    cpe:
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<rect x="3" y="11" width="18" height="8" rx="1.5"/>' +
      '<circle cx="7"  cy="15" r=".7" fill="currentColor"/>' +
      '<circle cx="11" cy="15" r=".7" fill="currentColor"/>' +
      '<circle cx="15" cy="15" r=".7" fill="currentColor"/>' +
      '<path d="M7 8 L7 4 M12 8 L12 3 M17 8 L17 4"/>' +
      "</svg>",
    // object, multi-instance grid (2x2 boxes)
    object:
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<rect x="3"  y="3"  width="8" height="8" rx="1"/>' +
      '<rect x="13" y="3"  width="8" height="8" rx="1"/>' +
      '<rect x="3"  y="13" width="8" height="8" rx="1"/>' +
      '<rect x="13" y="13" width="8" height="8" rx="1"/>' +
      "</svg>",
    // param, leaf / key-value tag
    param:
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<path d="M3 8 L3 14 L11 22 L21 12 L13 4 L3 4 Z"/>' +
      '<circle cx="7" cy="8" r="1.2" fill="currentColor"/>' +
      "</svg>",
    // generator, fallback (sine wave)
    generator:
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">' +
      '<path d="M2 12 Q 5 5 8 12 T 14 12 T 20 12 L22 12"/>' +
      "</svg>",
    // counter, odometer style three-digit chip
    "gen-counter":
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<rect x="3" y="7" width="18" height="10" rx="1.5"/>' +
      '<path d="M9 7 L9 17 M15 7 L15 17"/>' +
      '<path d="M5.5 11 L5.5 13 M11.5 11 L11.5 13 M17.5 11 L17.5 13"/>' +
      "</svg>",
    // drift, jagged drift line
    "gen-drift":
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">' +
      '<path d="M2 13 L6 9 L9 15 L13 7 L17 14 L22 11"/>' +
      "</svg>",
    // enum, bulleted list (cycles through values)
    "gen-enum":
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<circle cx="5" cy="6"  r="1.4" fill="currentColor"/>' +
      '<circle cx="5" cy="12" r="1.4" fill="currentColor"/>' +
      '<circle cx="5" cy="18" r="1.4" fill="currentColor"/>' +
      '<path d="M9 6 L20 6 M9 12 L20 12 M9 18 L20 18"/>' +
      "</svg>",
    // uptime, clock with monotonic up tick
    "gen-uptime":
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<circle cx="11" cy="13" r="8"/>' +
      '<path d="M11 8 L11 13 L15 15"/>' +
      '<path d="M16 6 L19 3 L22 6 M19 3 L19 9"/>' +
      "</svg>",
    // wallclock, clock face
    "gen-wallclock":
      '<svg viewBox="0 0 24 24" fill="none" aria-hidden="true" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">' +
      '<circle cx="12" cy="12" r="9"/>' +
      '<path d="M12 6 L12 12 L17 12"/>' +
      '<path d="M12 3 L12 4 M12 20 L12 21 M3 12 L4 12 M20 12 L21 12"/>' +
      "</svg>",
  };

  // Domain-specific label pools, each level cycles through cpe-labs
  // concepts so the topology reads as the actual product surface.
  const OBJECTS = [
    { name: "Device.WiFi.SSID",                sub: "Object · 4 inst" },
    { name: "Device.Hosts.Host",               sub: "Object · 12 inst" },
    { name: "Device.IP.Interface",             sub: "Object · 3 inst" },
    { name: "Device.Ethernet.Stats",           sub: "Group" },
    { name: "Device.WiFi.Radio",               sub: "Object · 2 inst" },
    { name: "Device.LocalAgent.Subscription",  sub: "Object · 4 inst" },
    { name: "Device.Hosts.Host",               sub: "Object · 12 inst" },
    { name: "Device.WAN.PPP.Interface",        sub: "Object · 1 inst" },
  ];
  // Generator type registry, icon + accent color per kind. Sub (interval +
  // jitter) gets attached per-param so the same kind reads with realistic
  // variety across the leaf row.
  const GEN_TYPES = {
    counter:   { icon: "gen-counter",   color: "#2563EB" },
    drift:     { icon: "gen-drift",     color: "#10B981" },
    enum:      { icon: "gen-enum",      color: "#F59E0B" },
    uptime:    { icon: "gen-uptime",    color: "#8B5CF6" },
    wallclock: { icon: "gen-wallclock", color: "#0EA5E9" },
  };

  // PARAMS map each parameter to its semantically-valid generators, so the
  // leaf row never shows a drift on an IP address or an enum on a byte
  // counter. Each param ships with two compatible generator configs (the
  // topology renders 2 generators per param).
  const PARAMS = [
    { name: "BytesSent", sub: "xsd:unsignedInt", gens: [
      { type: "counter", sub: "30s · ±20%" },
      { type: "counter", sub: "10s · ±5%" },
    ]},
    { name: "BytesReceived", sub: "xsd:unsignedInt", gens: [
      { type: "counter", sub: "30s · ±15%" },
      { type: "counter", sub: "5s · ±10%" },
    ]},
    { name: "PacketsSent", sub: "xsd:unsignedInt", gens: [
      { type: "counter", sub: "60s · ±25%" },
      { type: "counter", sub: "30s · ±10%" },
    ]},
    { name: "TotalAssociations", sub: "xsd:unsignedInt", gens: [
      { type: "counter", sub: "5m · ±10%" },
      { type: "counter", sub: "1m · ±5%" },
    ]},
    { name: "RSSI", sub: "xsd:int", gens: [
      { type: "drift", sub: "60s · ±3 dBm" },
      { type: "drift", sub: "30s · ±5 dBm" },
    ]},
    { name: "SignalStrength", sub: "xsd:int", gens: [
      { type: "drift", sub: "30s · ±2 dBm" },
      { type: "drift", sub: "120s · ±4 dBm" },
    ]},
    { name: "TxPower", sub: "xsd:int", gens: [
      { type: "drift", sub: "5m · ±1 dB" },
      { type: "drift", sub: "1m · ±2 dB" },
    ]},
    { name: "Status", sub: "xsd:string", gens: [
      { type: "enum", sub: "5m · cycle" },
      { type: "enum", sub: "1m · random" },
    ]},
    { name: "Active", sub: "xsd:boolean", gens: [
      { type: "enum", sub: "10m · cycle" },
      { type: "enum", sub: "30s · random" },
    ]},
    { name: "Channel", sub: "xsd:unsignedInt", gens: [
      { type: "enum", sub: "5m · cycle" },
      { type: "drift", sub: "60s · ±1" },
    ]},
    { name: "UpTime", sub: "xsd:unsignedInt", gens: [
      { type: "uptime", sub: "1s · monotonic" },
      { type: "uptime", sub: "5s · monotonic" },
    ]},
    { name: "CurrentLocalTime", sub: "xsd:dateTime", gens: [
      { type: "wallclock", sub: "1s · UTC" },
      { type: "wallclock", sub: "10s · UTC" },
    ]},
  ];

  function buildTree() {
    const nodes = [];
    const edges = [];
    let cpeSeq = 0, objSeq = 0, paramSeq = 0, genSeq = 0;

    function place(level, parentIdx, parentX) {
      if (level >= CONFIG.length) return;
      const cfg = CONFIG[level];
      const count = level === 0 ? 1 : cfg.branch;
      for (let i = 0; i < count; i++) {
        const t = count === 1 ? 0.5 : i / (count - 1);
        const x = count === 1 ? parentX : parentX + (t - 0.5) * cfg.xSpan;
        const idx = nodes.length;

        let label, sub;
        if (cfg.kind === "root") {
          label = "profile";
          sub = "vendor.yaml · fleet";
        } else if (cfg.kind === "cpe") {
          cpeSeq++;
          label = `cpe-${String(cpeSeq).padStart(3, "0")}`;
          sub = "TR-069 · TR-369";
        } else if (cfg.kind === "object") {
          const o = OBJECTS[objSeq++ % OBJECTS.length];
          label = o.name;
          sub = o.sub;
        } else if (cfg.kind === "param") {
          const p = PARAMS[paramSeq++ % PARAMS.length];
          label = p.name;
          sub = p.sub;
          var paramGens = p.gens;
        } else if (cfg.kind === "generator") {
          // Pick from the parent param's compatible generator list so the
          // type/param pairing always reads as plausible (counter on byte
          // counters, drift on RSSI, enum on Status, etc).
          const parent = nodes[parentIdx];
          const parentGens = (parent && parent.paramGens) || [];
          const g = parentGens[genSeq++ % Math.max(parentGens.length, 1)];
          if (g) {
            label = g.type;
            sub = g.sub;
            var iconKey = GEN_TYPES[g.type] && GEN_TYPES[g.type].icon;
            var iconColor = GEN_TYPES[g.type] && GEN_TYPES[g.type].color;
          } else {
            label = "generator";
            sub = "";
          }
        }

        nodes.push({
          x, y: cfg.y, idx, level, parentIdx, kind: cfg.kind, label, sub,
          iconKey: iconKey,
          iconColor: iconColor,
          paramGens: paramGens,
        });
        if (parentIdx >= 0) {
          edges.push({ from: parentIdx, to: idx, fromNode: nodes[parentIdx], toNode: nodes[idx] });
        }
        place(level + 1, idx, x);
      }
    }
    place(0, -1, 0);
    return { nodes, edges };
  }

  function smoothstepPathD(a, b, radius) {
    radius = radius == null ? 14 : radius;
    const dx = b.x - a.x;
    const dy = b.y - a.y;

    if (Math.abs(dx) < 0.5) {
      return `M ${a.x} ${a.y} L ${b.x} ${b.y}`;
    }

    const midY = a.y + dy * 0.5;
    const r = Math.min(radius, Math.abs(dx) * 0.5, Math.abs(dy) * 0.5);
    const dirX = dx > 0 ? 1 : -1;

    return [
      `M ${a.x} ${a.y}`,
      `L ${a.x} ${midY - r}`,
      `Q ${a.x} ${midY} ${a.x + r * dirX} ${midY}`,
      `L ${b.x - r * dirX} ${midY}`,
      `Q ${b.x} ${midY} ${b.x} ${midY + r}`,
      `L ${b.x} ${b.y}`,
    ].join(" ");
  }

  function easeInOutCubic(t) {
    return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;
  }
  function lerp(a, b, t) { return a + (b - a) * t; }

  function init() {
    const stage = document.getElementById("hero-tree");
    if (!stage) return;
    if (window.innerWidth < 1024) return;

    const camera = stage.querySelector(".hero-stage-inner");
    if (!camera) return;

    const TILT_X_DEG = 32;
    const TILT_Z_DEG = -18;
    const OVERVIEW = { x: 0, y: 315, scale: 0.72 };
    const ROOT_POSE = { x: 0, y: 0, scale: 1.35 };
    camera.style.transform =
      `rotateZ(${TILT_Z_DEG}deg) rotateX(${TILT_X_DEG}deg) scale(${ROOT_POSE.scale}) translate(${(-ROOT_POSE.x).toFixed(2)}px, ${(-ROOT_POSE.y).toFixed(2)}px)`;

    const { nodes, edges } = buildTree();

    const svgNS = "http://www.w3.org/2000/svg";
    const minX = -560, maxX = 560, minY = -40, maxY = 700;
    const svg = document.createElementNS(svgNS, "svg");
    svg.setAttribute("class", "tree-edges");
    svg.setAttribute("viewBox", `${minX} ${minY} ${maxX - minX} ${maxY - minY}`);
    svg.setAttribute("aria-hidden", "true");
    svg.style.cssText =
      `position:absolute;left:${minX}px;top:${minY}px;` +
      `width:${maxX - minX}px;height:${maxY - minY}px;` +
      `overflow:visible;pointer-events:none;`;
    camera.appendChild(svg);

    // Map (fromIdx -> toIdx -> path element) so we can hide the static gray
    // edge under each marching-dotted active edge (otherwise the gray line
    // bleeds through the gaps in the dash pattern).
    const bgEdgeMap = new Map();
    edges.forEach((e) => {
      const path = document.createElementNS(svgNS, "path");
      path.setAttribute("class", "tree-edge");
      path.setAttribute("d", smoothstepPathD(e.fromNode, e.toNode));
      svg.appendChild(path);
      let inner = bgEdgeMap.get(e.from);
      if (!inner) { inner = new Map(); bgEdgeMap.set(e.from, inner); }
      inner.set(e.to, path);
    });

    const activeEdgeEls = [];
    const pulseEdgeEls = [];
    for (let i = 0; i < CONFIG.length - 1; i++) {
      const path = document.createElementNS(svgNS, "path");
      path.setAttribute("class", "tree-edge-active");
      path.setAttribute("d", "M0 0");
      svg.appendChild(path);
      activeEdgeEls.push({ el: path, len: 0 });
    }
    // Pulse layer drawn AFTER all solid blue edges so all pulses sit on top.
    for (let i = 0; i < CONFIG.length - 1; i++) {
      const pulse = document.createElementNS(svgNS, "path");
      pulse.setAttribute("class", "tree-edge-pulse");
      pulse.setAttribute("d", "M0 0");
      pulse.setAttribute("pathLength", "100");
      svg.appendChild(pulse);
      pulseEdgeEls.push(pulse);
    }

    const nodeEls = nodes.map((n) => {
      const outer = document.createElement("div");
      outer.className = `tree-node tree-node-${n.kind}`;
      outer.dataset.idx = n.idx;
      outer.style.transform = `translate3d(${n.x}px, ${n.y}px, 0) translate(-50%, -50%)`;

      const inner = document.createElement("div");
      inner.className = "tree-node-inner";
      const iconSvg = ICONS[n.iconKey] || ICONS[n.kind];
      const iconStyle = n.iconColor ? ` style="color:${n.iconColor}"` : "";
      inner.innerHTML =
        `<span class="tree-node-icon"${iconStyle}>${iconSvg}</span>` +
        `<span class="tree-node-text">` +
          `<span class="tree-node-name">${n.label}</span>` +
          `<span class="tree-node-sub">${n.sub}</span>` +
        `</span>`;
      outer.appendChild(inner);
      camera.appendChild(outer);
      return outer;
    });

    const leaves = nodes.filter((n) => n.level === LEAF_LEVEL);
    function pickChain(prevLeaf) {
      let leaf = leaves[Math.floor(Math.random() * leaves.length)];
      let attempts = 0;
      while (prevLeaf && leaf === prevLeaf && attempts++ < 8) {
        leaf = leaves[Math.floor(Math.random() * leaves.length)];
      }
      const path = [leaf];
      let cur = leaf;
      while (cur.parentIdx >= 0) {
        cur = nodes[cur.parentIdx];
        path.push(cur);
      }
      return path.reverse();
    }
    let activeChain = pickChain(null);

    function applyActiveEdges(chain) {
      // Restore visibility on every static edge first (the previous chain's
      // hidden edges need to come back).
      bgEdgeMap.forEach((inner) => {
        inner.forEach((p) => { p.style.opacity = ""; });
      });

      // Briefly hide the active + pulse edges before reassigning their d=,
      // then fade them back in. Without this, swapping the path geometry
      // produces a jarring snap. The transition: opacity rule on
      // .tree-edge-active and .tree-edge-pulse makes this smooth.
      for (let i = 0; i < CONFIG.length - 1; i++) {
        activeEdgeEls[i].el.style.opacity = "0";
        pulseEdgeEls[i].style.opacity = "0";
      }
      // Reassign d= on the next frame so the opacity:0 state takes effect
      // before the geometry swap; then fade back in on the frame after.
      requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          for (let i = 0; i < CONFIG.length - 1; i++) {
            activeEdgeEls[i].el.style.opacity = "";
            pulseEdgeEls[i].style.opacity = "";
          }
        });
      });

      for (let i = 0; i < CONFIG.length - 1; i++) {
        const a = chain[i], b = chain[i + 1];
        const d = smoothstepPathD(a, b);
        activeEdgeEls[i].el.setAttribute("d", d);
        pulseEdgeEls[i].setAttribute("d", d);

        // Hide the static gray edge that sits behind this active edge so
        // the solid blue + white pulse have a clean background.
        const innerMap = bgEdgeMap.get(a.idx);
        const bg = innerMap && innerMap.get(b.idx);
        if (bg) bg.style.opacity = "0";
      }
    }
    applyActiveEdges(activeChain);

    function markActiveNodes(chain) {
      nodeEls.forEach((el) => el.classList.remove("in-path"));
      chain.forEach((n) => nodeEls[n.idx].classList.add("in-path"));
    }
    markActiveNodes(activeChain);

    function buildKeyframes(chain) {
      const SCALES = { root: 1.35, cpe: 1.5, object: 1.7, param: 1.9, generator: 2.1 };
      const HOLD   = { overview: 1.1, root: 0.85, cpe: 0.8, object: 0.85, param: 0.85, generator: 1.1 };
      const TRANS  = { in: 1.35, between: 1.05, out: 1.55 };

      const kfs = [];
      let t = 0;
      const rootPose = { x: chain[0].x, y: chain[0].y, scale: SCALES.root, focusIdx: chain[0].idx };

      kfs.push({ time: t, ...rootPose });
      t += HOLD.root;
      kfs.push({ time: t, ...rootPose });

      const stages = ["cpe", "object", "param", "generator"];
      for (let i = 0; i < stages.length; i++) {
        const node = chain[i + 1];
        const sc = SCALES[stages[i]];
        const hold = HOLD[stages[i]];
        t += TRANS.between;
        kfs.push({ time: t, x: node.x, y: node.y, scale: sc, focusIdx: node.idx });
        t += hold;
        kfs.push({ time: t, x: node.x, y: node.y, scale: sc, focusIdx: node.idx });
      }

      t += TRANS.out;
      kfs.push({ time: t, ...OVERVIEW, focusIdx: -1 });
      t += HOLD.overview;
      kfs.push({ time: t, ...OVERVIEW, focusIdx: -1 });

      t += TRANS.in;
      kfs.push({ time: t, ...rootPose });

      return kfs;
    }
    let keyframes = buildKeyframes(activeChain);
    let cycleDuration = keyframes[keyframes.length - 1].time;

    function getCamera(t) {
      let prev = keyframes[0], next = keyframes[1];
      for (let i = 0; i < keyframes.length - 1; i++) {
        if (t >= keyframes[i].time && t < keyframes[i + 1].time) {
          prev = keyframes[i]; next = keyframes[i + 1]; break;
        }
      }
      const dur = next.time - prev.time || 1;
      const localT = Math.max(0, Math.min(1, (t - prev.time) / dur));
      const e = easeInOutCubic(localT);
      return {
        x: lerp(prev.x, next.x, e),
        y: lerp(prev.y, next.y, e),
        scale: lerp(prev.scale, next.scale, e),
        focusIdx: localT > 0.6 ? next.focusIdx : prev.focusIdx,
      };
    }

    const mouse = { x: 0, y: 0, tx: 0, ty: 0 };
    stage.addEventListener("pointermove", (ev) => {
      const r = stage.getBoundingClientRect();
      mouse.tx = ((ev.clientX - r.left) / r.width  - 0.5);
      mouse.ty = ((ev.clientY - r.top)  / r.height - 0.5);
    });
    stage.addEventListener("pointerleave", () => { mouse.tx = 0; mouse.ty = 0; });

    let visible = true;
    new IntersectionObserver(
      (es) => { visible = es[0].isIntersecting; },
      { threshold: 0.01 }
    ).observe(stage);

    let lastFocused = -1;
    let cycleStart = performance.now();

    function tick() {
      requestAnimationFrame(tick);
      if (!visible) return;
      const now = performance.now();
      const t = (now - cycleStart) / 1000;

      if (t >= cycleDuration) {
        cycleStart = now;
        const lastLeaf = activeChain[activeChain.length - 1];
        activeChain = pickChain(lastLeaf);
        keyframes = buildKeyframes(activeChain);
        cycleDuration = keyframes[keyframes.length - 1].time;
        applyActiveEdges(activeChain);
        markActiveNodes(activeChain);
        return;
      }

      const cam = getCamera(t);

      mouse.x += (mouse.tx - mouse.x) * 0.06;
      mouse.y += (mouse.ty - mouse.y) * 0.06;
      const parallaxX = mouse.x * 22 / cam.scale;
      const parallaxY = mouse.y * 12 / cam.scale;

      camera.style.transform =
        `rotateZ(${TILT_Z_DEG}deg) rotateX(${TILT_X_DEG}deg) scale(${cam.scale}) translate(${(-cam.x + parallaxX).toFixed(2)}px, ${(-cam.y + parallaxY).toFixed(2)}px)`;

      if (cam.focusIdx !== lastFocused) {
        if (lastFocused >= 0 && nodeEls[lastFocused]) nodeEls[lastFocused].classList.remove("focused");
        if (cam.focusIdx >= 0 && nodeEls[cam.focusIdx]) nodeEls[cam.focusIdx].classList.add("focused");
        lastFocused = cam.focusIdx;
      }

      // Edge marching is driven by CSS @keyframes edge-march (continuous);
      // no per-frame JS update needed.
    }
    tick();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();
