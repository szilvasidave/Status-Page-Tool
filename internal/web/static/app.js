// Small progressive enhancements. No inline scripts are allowed (CSP).
(function () {
  "use strict";

  // <form data-confirm="Really delete?"> asks before submitting.
  // <form data-unsaved="Changes will be lost. Continue?"> notes when one of
  // its fields is changed; a button inside it that submits another form
  // (form="…") then asks first, because that reloads the page without the
  // changes (e.g. "Generate token" in the settings).
  function noteChange(e) {
    var f = e.target.form;
    if (f && f.hasAttribute("data-unsaved")) f.setAttribute("data-changed", "");
  }
  document.addEventListener("input", noteChange);
  document.addEventListener("change", noteChange);
  document.addEventListener("submit", function (e) {
    var outer = e.submitter && e.submitter.closest("form[data-changed]");
    if (outer && outer !== e.target && !window.confirm(outer.getAttribute("data-unsaved"))) {
      e.preventDefault();
      return;
    }
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
  // Some browsers report the old (CLDR) names of renamed zones.
  var renamedZones = {
    "Asia/Calcutta": "Asia/Kolkata", "Asia/Saigon": "Asia/Ho_Chi_Minh", "Asia/Katmandu": "Asia/Kathmandu",
    "Asia/Rangoon": "Asia/Yangon", "Europe/Kiev": "Europe/Kyiv", "America/Godthab": "America/Nuuk",
    "America/Buenos_Aires": "America/Argentina/Buenos_Aires", "America/Catamarca": "America/Argentina/Catamarca",
    "America/Cordoba": "America/Argentina/Cordoba", "America/Jujuy": "America/Argentina/Jujuy",
    "America/Mendoza": "America/Argentina/Mendoza", "America/Indianapolis": "America/Indiana/Indianapolis",
    "America/Louisville": "America/Kentucky/Louisville", "America/Coral_Harbour": "America/Atikokan",
    "Atlantic/Faeroe": "Atlantic/Faroe", "Africa/Asmera": "Africa/Asmara", "Pacific/Truk": "Pacific/Chuuk",
    "Pacific/Ponape": "Pacific/Pohnpei", "Pacific/Enderbury": "Pacific/Kanton"
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
