/* Adds a "Powered by ispx" block: pinned to the top left corner of the
   header on screens wide enough to have one, at the top of the sidebar
   otherwise. CSS media queries pick which copy is visible. */
document.addEventListener("DOMContentLoaded", function () {
  var icon = document.querySelector("header a img[alt=icon]");

  function block(variant) {
    var link = document.createElement("a");
    link.className = "ispx-powered ispx-powered--" + variant;
    link.href = "https://ispx.co";
    link.target = "_blank";
    link.rel = "noreferrer";
    if (icon) {
      var img = document.createElement("img");
      img.src = icon.src;
      img.alt = "ispx";
      link.appendChild(img);
    }
    var text = document.createElement("span");
    text.className = "ispx-powered__text";
    var label = document.createElement("span");
    label.className = "ispx-powered__label";
    label.textContent = "Powered by";
    var name = document.createElement("span");
    name.className = "ispx-powered__name";
    name.textContent = "ispx";
    text.appendChild(label);
    text.appendChild(name);
    link.appendChild(text);
    return link;
  }

  var header = document.querySelector("header");
  var corner;
  if (header) {
    corner = block("corner");
    header.appendChild(corner);
  }
  var sidebar = document.querySelector('[data-slot="sidebar-content"]');
  if (sidebar) {
    sidebar.insertBefore(block("sidebar"), sidebar.firstChild);
  }

  // Align the corner block's left edge with the sidebar menu below it.
  function align() {
    if (!corner) return;
    var menu = document.querySelector('[data-sidebar="menu"]');
    if (menu) {
      corner.style.left = menu.getBoundingClientRect().left + "px";
    }
  }
  align();
  window.addEventListener("resize", align);
});
