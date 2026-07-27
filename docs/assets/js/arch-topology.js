/**
 * Architecture topology, flat (non-isometric) version of the hero tree.
 *
 *   cmd/cpe-sim -> cperng / scheduler / generators / cwmp / cr -> paramtree
 *
 * Static layout, no camera fly-through. Solid blue edges with traveling
 * white pulses (fibre-light) to show data flow through the package graph.
 * Mirrors the hero's visual vocabulary so the two diagrams read as one
 * design system.
 */
(function () {
  if (typeof window === "undefined") return;

  const STAGE_W = 960;
  const STAGE_H = 360;

  const ICONS = {
    binary:
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<rect x="4" y="4" width="16" height="16" rx="2"/>' +
      '<path d="M9 9 L9 15 M15 9 L15 15"/>' +
      '<path d="M9 12 L15 12"/>' +
      "</svg>",
    rng:
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<rect x="4" y="4" width="16" height="16" rx="2"/>' +
      '<circle cx="8"  cy="8"  r="1.2" fill="currentColor"/>' +
      '<circle cx="16" cy="8"  r="1.2" fill="currentColor"/>' +
      '<circle cx="12" cy="12" r="1.2" fill="currentColor"/>' +
      '<circle cx="8"  cy="16" r="1.2" fill="currentColor"/>' +
      '<circle cx="16" cy="16" r="1.2" fill="currentColor"/>' +
      "</svg>",
    clock:
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<circle cx="12" cy="12" r="9"/>' +
      '<path d="M12 7 L12 12 L16 14"/>' +
      "</svg>",
    wave:
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<path d="M2 12 Q 5 5 8 12 T 14 12 T 20 12 L22 12"/>' +
      "</svg>",
    protocol:
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<rect x="3" y="6" width="18" height="12" rx="2"/>' +
      '<path d="M3 8 L12 14 L21 8"/>' +
      "</svg>",
    inbox:
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<path d="M3 13 L7 5 L17 5 L21 13 L21 19 L3 19 Z"/>' +
      '<path d="M3 13 L9 13 L10 15 L14 15 L15 13 L21 13"/>' +
      "</svg>",
    tree:
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      '<path d="M12 3 L12 8 M6 8 L18 8 L18 12 M6 8 L6 12"/>' +
      '<rect x="3" y="12" width="6" height="6" rx="1"/>' +
      '<rect x="9" y="12" width="6" height="6" rx="1"/>' +
      '<rect x="15" y="12" width="6" height="6" rx="1"/>' +
      "</svg>",
  };

  const NODES = [
    { id: "main",       x: 480, y: 36,  kind: "root", label: "cmd/cpe-sim",  sub: "binary · CLI · fleet wiring", icon: "binary"  },
    { id: "cperng",     x: 90,  y: 180, kind: "pkg",  label: "cperng",       sub: "per-CPE RNG",                  icon: "rng"     },
    { id: "scheduler",  x: 285, y: 180, kind: "pkg",  label: "scheduler",    sub: "periodic Inform",              icon: "clock"   },
    { id: "generators", x: 480, y: 180, kind: "pkg",  label: "generators",   sub: "counter · drift · enum",       icon: "wave"    },
    { id: "cwmp",       x: 675, y: 180, kind: "pkg",  label: "cwmp",         sub: "TR-069 / TR-369",              icon: "protocol"},
    { id: "cr",         x: 870, y: 180, kind: "pkg",  label: "cr",           sub: "CR listener",                  icon: "inbox"   },
    { id: "paramtree",  x: 480, y: 320, kind: "spine",label: "paramtree",    sub: "in-memory tree · BBF types",   icon: "tree"    },
  ];

  const EDGES = [
    ["main", "cperng"], ["main", "scheduler"], ["main", "generators"], ["main", "cwmp"], ["main", "cr"],
    ["cperng", "paramtree"], ["scheduler", "paramtree"], ["generators", "paramtree"], ["cwmp", "paramtree"], ["cr", "paramtree"],
  ];

  function smoothstepPathD(a, b, radius) {
    radius = radius == null ? 14 : radius;
    const dx = b.x - a.x;
    const dy = b.y - a.y;
    if (Math.abs(dx) < 0.5) return `M ${a.x} ${a.y} L ${b.x} ${b.y}`;
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

  function init() {
    const stage = document.getElementById("arch-topology");
    if (!stage) return;
    if (stage.dataset.initialized) return;
    stage.dataset.initialized = "1";

    stage.style.position = "relative";
    stage.style.width = "100%";
    stage.style.maxWidth = STAGE_W + "px";
    stage.style.aspectRatio = `${STAGE_W} / ${STAGE_H}`;
    stage.style.margin = "1.5rem auto";

    const svgNS = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(svgNS, "svg");
    svg.setAttribute("viewBox", `0 0 ${STAGE_W} ${STAGE_H}`);
    svg.setAttribute("class", "arch-svg");
    svg.style.cssText = "position:absolute;inset:0;width:100%;height:100%;overflow:visible;pointer-events:none;";
    stage.appendChild(svg);

    const nodeById = {};
    NODES.forEach((n) => { nodeById[n.id] = n; });

    EDGES.forEach((e) => {
      const a = nodeById[e[0]], b = nodeById[e[1]];
      const d = smoothstepPathD(a, b);

      const solid = document.createElementNS(svgNS, "path");
      solid.setAttribute("class", "arch-edge");
      solid.setAttribute("d", d);
      svg.appendChild(solid);
    });

    NODES.forEach((n) => {
      const el = document.createElement("div");
      el.className = `arch-node arch-node-${n.kind}`;
      // Stage uses a viewBox-like coordinate space; nodes position via %.
      el.style.left = ((n.x / STAGE_W) * 100).toFixed(3) + "%";
      el.style.top  = ((n.y / STAGE_H) * 100).toFixed(3) + "%";
      el.innerHTML =
        `<span class="arch-node-icon">${ICONS[n.icon] || ""}</span>` +
        `<span class="arch-node-text">` +
          `<span class="arch-node-name">${n.label}</span>` +
          `<span class="arch-node-sub">${n.sub}</span>` +
        `</span>`;
      stage.appendChild(el);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();
