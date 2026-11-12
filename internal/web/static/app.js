// Small progressive enhancements. No inline scripts are allowed (CSP).
(function () {
  "use strict";

  // <form data-confirm="Really delete?"> asks before submitting.
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
    }
  });

  // <select data-show-when="value" data-target="#id"> (or a checkbox with
  // data-show-when="on") shows #id only when the value matches.
  function syncToggles() {
    document.querySelectorAll("[data-show-when]").forEach(function (sel) {
      var target = document.querySelector(sel.getAttribute("data-target"));
      if (!target) return;
      var want = sel.getAttribute("data-show-when").split(",");
      var val = sel.type === "checkbox" ? (sel.checked ? "on" : "off") : sel.value;
      target.hidden = want.indexOf(val) === -1;
    });
  }
  document.addEventListener("change", syncToggles);
  document.addEventListener("DOMContentLoaded", syncToggles);
})();
