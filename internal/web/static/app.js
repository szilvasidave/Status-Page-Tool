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

  // <input type="color" data-color-for="id" hidden> (branding page) is a
  // colour picker for the hex code in the text input #id: it is shown, starts
  // with that colour (or the input's data-default, the colour an empty field
  // stands for) and the two stay in sync. Picking a colour fires an input
  // event on the text input, so data-unsaved notices it.
  var hexCode = /^#?([0-9a-f]{3}|[0-9a-f]{6})$/i;
  function expandHex(v) {
    var m = hexCode.exec((v || "").trim());
    if (!m) return "";
    var h = m[1].toLowerCase();
    if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
    return "#" + h;
  }
  function setupColorPickers() {
    document.querySelectorAll("input[type=color][data-color-for]").forEach(function (picker) {
      var text = document.getElementById(picker.getAttribute("data-color-for"));
      if (!text) return;
      function fromText() {
        picker.value = expandHex(text.value) || expandHex(text.getAttribute("data-default")) || "#000000";
      }
      fromText();
      picker.hidden = false;
      picker.addEventListener("input", function () {
        text.value = picker.value;
        text.dispatchEvent(new Event("input", { bubbles: true }));
      });
      text.addEventListener("input", function () {
        if (expandHex(text.value) || text.value.trim() === "") fromText();
      });
    });
  }
  document.addEventListener("DOMContentLoaded", setupColorPickers);

  // <input type="file" data-max-bytes="N"> refuses a file larger than N
  // bytes when it is chosen, instead of after the upload.
  document.addEventListener("change", function (e) {
    var input = e.target;
    if (!input.matches || !input.matches("input[type=file][data-max-bytes]")) return;
    var max = parseInt(input.getAttribute("data-max-bytes"), 10);
    var file = input.files && input.files[0];
    if (file && max > 0 && file.size > max) {
      input.setCustomValidity("The file is too large (" + Math.ceil(file.size / 1024) + " KB); at most " +
        Math.floor(max / 1024) + " KB.");
      input.reportValidity();
    } else {
      input.setCustomValidity("");
    }
  });
})();
