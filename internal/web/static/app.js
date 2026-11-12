// Small progressive enhancements. No inline scripts are allowed (CSP).
(function () {
  "use strict";

  // <form data-confirm="Really delete?"> or <button data-confirm="..."> asks before submitting.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    var btn = e.submitter;
    var msg = (btn && btn.getAttribute("data-confirm")) || form.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
    }
  });

  // <input type="checkbox" data-toggle-all="name"> toggles all checkboxes with that name.
  document.addEventListener("change", function (e) {
    var el = e.target;
    var name = el.getAttribute && el.getAttribute("data-toggle-all");
    if (!name) return;
    document.querySelectorAll('input[type="checkbox"][name="' + name + '"]').forEach(function (cb) {
      cb.checked = el.checked;
    });
  });

  // <select data-show-when="value" data-target="#id"> shows #id only when the value matches.
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
