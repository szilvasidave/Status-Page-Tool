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
  // event on the text input, so data-unsaved notices it; a changed
  // data-default is taken over on a "defaultchange" event.
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
      text.addEventListener("defaultchange", function () {
        if (text.value.trim() === "") fromText();
      });
    });
  }
  document.addEventListener("DOMContentLoaded", setupColorPickers);

  // <input data-fallback-for="id" data-fallback-default="#…" data-fallback-bg="#…">
  // (the dark accent of the branding page): while #id (the light accent) is
  // edited, the colour an empty field stands for (placeholder and
  // data-default) follows it: the light accent lightened until it is visible
  // on the dark background, or data-fallback-default while #id is empty.
  //
  // The colour maths are a port of package theme (internal/theme/color.go:
  // luminance, Contrast, mix, ensureContrast; the dark accent rule of
  // Resolve in theme.go): keep them in sync, so that the default shown here
  // is the one the server uses after saving.
  function rgbOf(hex) {
    return [1, 3, 5].map(function (i) { return parseInt(hex.substr(i, 2), 16); });
  }
  function luminance(hex) {
    var lin = rgbOf(hex).map(function (v) {
      var f = v / 255;
      return f <= 0.04045 ? f / 12.92 : Math.pow((f + 0.055) / 1.055, 2.4);
    });
    return 0.2126 * lin[0] + 0.7152 * lin[1] + 0.0722 * lin[2];
  }
  function contrast(a, b) {
    var la = luminance(a), lb = luminance(b);
    return la < lb ? (lb + 0.05) / (la + 0.05) : (la + 0.05) / (lb + 0.05);
  }
  function mix(a, b, t) { // t = share of a, like theme's mix
    var ca = rgbOf(a), cb = rgbOf(b);
    return "#" + ca.map(function (v, i) {
      var n = Math.round(v * t + cb[i] * (1 - t));
      return (n < 16 ? "0" : "") + n.toString(16);
    }).join("");
  }
  function ensureContrast(c, bg, min, towards) {
    if (contrast(c, bg) >= min) return c;
    for (var i = 1; i <= 50; i++) {
      var r = mix(towards, c, i / 50);
      if (contrast(r, bg) >= min) return r;
    }
    return towards;
  }
  function darkAccentFallback(light, bg, def) {
    return light ? ensureContrast(light, bg, 3, "#ffffff") : def;
  }
  function setupFallbacks() {
    document.querySelectorAll("input[data-fallback-for]").forEach(function (input) {
      var from = document.getElementById(input.getAttribute("data-fallback-for"));
      if (!from) return;
      from.addEventListener("input", function () {
        var light = expandHex(from.value);
        if (!light && from.value.trim() !== "") return; // while typing: keep the last valid colour
        var fb = darkAccentFallback(light, input.getAttribute("data-fallback-bg"), input.getAttribute("data-fallback-default"));
        input.setAttribute("data-default", fb);
        input.placeholder = input.placeholder.replace(/#[0-9a-f]{6}/i, fb);
        input.dispatchEvent(new Event("defaultchange"));
      });
    });
  }
  document.addEventListener("DOMContentLoaded", setupFallbacks);

  // <input type="file" data-max-bytes="N"> refuses a file larger than N
  // bytes when it is chosen, instead of after the upload: the choice is
  // cleared (the form can still be saved) and a message is shown below the
  // field (a .field-error after its help text, announced and referenced by
  // aria-describedby). Choosing another file removes the message again.
  document.addEventListener("change", function (e) {
    var input = e.target;
    if (!input.matches || !input.matches("input[type=file][data-max-bytes]")) return;
    var max = parseInt(input.getAttribute("data-max-bytes"), 10);
    var file = input.files && input.files[0];
    var tooLarge = file && max > 0 && file.size > max;
    var id = input.id + "_size_error";
    var msg = document.getElementById(id);
    if (!msg && !tooLarge) return;
    if (!msg) {
      msg = document.createElement("span");
      msg.id = id;
      msg.className = "field-error";
      msg.setAttribute("role", "alert");
      var help = input.nextElementSibling;
      (help && help.classList.contains("help") ? help : input).after(msg);
    }
    var described = (input.getAttribute("aria-describedby") || "").split(/\s+/).filter(function (v) {
      return v && v !== id;
    });
    msg.hidden = !tooLarge;
    if (tooLarge) {
      msg.textContent = "The file is too large (" + Math.ceil(file.size / 1024) + " KB); at most " +
        Math.floor(max / 1024) + " KB. Choose a smaller file.";
      input.value = "";
      input.setAttribute("aria-invalid", "true");
      described.push(id);
    } else {
      msg.textContent = "";
      input.removeAttribute("aria-invalid");
    }
    if (described.length) input.setAttribute("aria-describedby", described.join(" "));
    else input.removeAttribute("aria-describedby");
  });
})();
