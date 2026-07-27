/**
 * Per-code-block copy button.
 *
 * The shadcn theme ships a "Copy Page" button (whole-markdown-source) but
 * not a per-block copy. Inject one onto every <pre> block so users can
 * grab snippets without selection gymnastics.
 */
(function () {
  if (typeof window === "undefined") return;

  const ICON_COPY =
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<rect x="9" y="9" width="11" height="11" rx="2"/>' +
    '<path d="M5 15 V6 a2 2 0 0 1 2 -2 h9"/>' +
    "</svg>";
  const ICON_OK =
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M5 12 L10 17 L20 7"/>' +
    "</svg>";

  function attach(pre) {
    if (pre.dataset.copyAttached) return;
    pre.dataset.copyAttached = "1";

    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "cpelabs-copy-btn";
    btn.setAttribute("aria-label", "Copy code to clipboard");
    btn.innerHTML = ICON_COPY + "<span>Copy</span>";

    btn.addEventListener("click", async () => {
      // Use the <code> child if present so we don't grab line-number prefixes
      // or other chrome from highlighters.
      const codeEl = pre.querySelector("code") || pre;
      const text = codeEl.innerText.replace(/ /g, " ");
      try {
        await navigator.clipboard.writeText(text);
      } catch (_) {
        const ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand("copy"); } catch (_) {}
        document.body.removeChild(ta);
      }
      btn.dataset.state = "copied";
      btn.innerHTML = ICON_OK + "<span>Copied</span>";
      setTimeout(() => {
        delete btn.dataset.state;
        btn.innerHTML = ICON_COPY + "<span>Copy</span>";
      }, 1600);
    });

    pre.appendChild(btn);
  }

  function init() {
    document.querySelectorAll("article pre").forEach(attach);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();
