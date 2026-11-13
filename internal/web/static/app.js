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

  // <select data-browser-timezone="UTC"> (setup wizard) selects the
  // browser's time zone if it is offered and the select still shows UTC.
  // Some browsers report the old names of renamed zones.
  var renamedZones = {
    "Asia/Calcutta": "Asia/Kolkata", "Asia/Saigon": "Asia/Ho_Chi_Minh", "Asia/Katmandu": "Asia/Kathmandu",
    "Asia/Rangoon": "Asia/Yangon", "Europe/Kiev": "Europe/Kyiv", "America/Godthab": "America/Nuuk",
    "America/Buenos_Aires": "America/Argentina/Buenos_Aires", "America/Indianapolis": "America/Indiana/Indianapolis",
    "America/Louisville": "America/Kentucky/Louisville", "Atlantic/Faeroe": "Atlantic/Faroe", "Africa/Asmera": "Africa/Asmara"
  };
  function selectBrowserTimezone() {
    var zone;
    try {
      zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    } catch (e) {
      return;
    }
    zone = renamedZones[zone] || zone;
    document.querySelectorAll("select[data-browser-timezone]").forEach(function (sel) {
      if (!zone || sel.value !== sel.getAttribute("data-browser-timezone")) return;
      for (var i = 0; i < sel.options.length; i++) {
        if (sel.options[i].value === zone) {
          sel.value = zone;
          return;
        }
      }
    });
  }
  document.addEventListener("DOMContentLoaded", selectBrowserTimezone);
})();
