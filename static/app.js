// Small progressive enhancements on top of htmx.
(function () {
  "use strict";

  function showToast(msg, kind) {
    var box = document.getElementById("toasts");
    if (!box) return;
    var el = document.createElement("div");
    el.className = "toast toast-" + (kind || "success");
    el.dataset.autohide = "";
    el.textContent = msg;
    box.appendChild(el);
    autohide(el);
  }

  function autohide(el) {
    setTimeout(function () { el.classList.add("hide"); }, 3500);
    setTimeout(function () { el.remove(); }, 4000);
  }

  // Copy each column header into its cells' data-label: on phones the CSS
  // turns table rows into cards and shows these labels next to the values.
  // Runs for every .table, including rows added later by htmx or JS.
  function labelTables() {
    document.querySelectorAll("table.table").forEach(function (t) {
      var ths = t.querySelectorAll("thead th");
      var heads = Array.prototype.map.call(ths, function (th) {
        return th.textContent.trim();
      });
      alignHeaders(t, ths);
      t.querySelectorAll("tbody tr").forEach(function (tr) {
        Array.prototype.forEach.call(tr.children, function (td, i) {
          if (td.tagName === "TD" && !td.hasAttribute("data-label") && heads[i]) td.setAttribute("data-label", heads[i]);
        });
      });
    });
  }

  // A column header always takes the alignment of its cells: if every data
  // cell of a column is .num (right) or .center, so is its <th>. Rows with
  // colspan (group headers) are ignored. Safety net for the rule in
  // app.css, so a new table can't show numbers misaligned with their title.
  function alignHeaders(t, ths) {
    var rows = Array.prototype.filter.call(t.querySelectorAll("tbody tr"), function (tr) {
      return !tr.querySelector("td[colspan]");
    });
    if (!rows.length) return;
    Array.prototype.forEach.call(ths, function (th, i) {
      ["num", "center"].forEach(function (cls) {
        var all = rows.every(function (tr) {
          var td = tr.children[i];
          return td && td.classList.contains(cls);
        });
        th.classList.toggle(cls, all);
      });
    });
  }

  // Keep the active section visible in the horizontally scrolling mobile menu.
  function revealActiveNav() {
    var a = document.querySelector(".sidebar nav a.active");
    var nav = a && a.parentElement;
    if (nav && nav.scrollWidth > nav.clientWidth) {
      nav.scrollLeft = a.offsetLeft - (nav.clientWidth - a.offsetWidth) / 2;
    }
  }

  // Consegne summary bar: bounce once when it appears (not on every update
  // while it's already visible — htmx replaces it out-of-band each time).
  var selBarVisible = false;
  function checkSelBar() {
    var bar = document.getElementById("sel-bar");
    var visible = !!bar && bar.textContent.trim() !== "";
    if (visible && !selBarVisible) {
      bar.classList.remove("bounce");
      void bar.offsetWidth; // restart the animation
      bar.classList.add("bounce");
    }
    selBarVisible = visible;
  }

  // Tables with a sticky header (Clienti, Prodotti) scroll inside their own
  // box, which ends at the bottom of the window: tell CSS where the box starts.
  function fitStickyTables() {
    document.querySelectorAll(".table-wrap.sticky-head").forEach(function (w) {
      w.style.setProperty("--wrap-top", Math.round(w.getBoundingClientRect().top + window.scrollY) + "px");
    });
  }
  window.addEventListener("resize", fitStickyTables);

  function init(root) {
    labelTables();
    fitStickyTables();
    checkSelBar();
    restoreViews();
    root.querySelectorAll("[data-autohide]").forEach(function (el) {
      if (!el.dataset.bound) { el.dataset.bound = "1"; autohide(el); }
    });
    var flash = root.querySelector("#toasts-init");
    if (flash && flash.dataset.toast) { showToast(flash.dataset.toast); flash.remove(); }
  }

  document.addEventListener("DOMContentLoaded", function () { init(document); revealActiveNav(); });
  document.addEventListener("htmx:load", function (e) { init(e.target); revealActiveNav(); });

  // Sun/moon button: switch light/dark theme; the choice is remembered in this browser
  // (layout.templ applies it before the first paint).
  document.addEventListener("click", function (e) {
    if (!e.target.closest("[data-theme-toggle]")) return;
    var next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem("adesgo-theme", next); } catch (_) {}
  });

  // Autosave fields ([data-autosave], e.g. Clienti): htmx posts the field on
  // "change" (= when you leave it after editing). Saved → a toast at the
  // bottom right ("Salvato: Città · Mario Rossi"); refused → the field gets a
  // red border and the server's toast explains why.
  document.addEventListener("htmx:beforeRequest", function (e) {
    var el = e.detail.elt;
    if (!el.matches || !el.matches("[data-autosave]")) return;
    el.classList.remove("save-error");
    el.classList.add("saving");
  });
  document.addEventListener("htmx:afterRequest", function (e) {
    var el = e.detail.elt;
    if (!el.matches || !el.matches("[data-autosave]")) return;
    el.classList.remove("saving");
    var xhr = e.detail.xhr;
    var failed = !e.detail.successful || (xhr && xhr.getResponseHeader("X-Autosave-Error"));
    if (failed) {
      el.classList.add("save-error");
      if (!xhr || xhr.status === 0) showToast("Non salvato: connessione assente. Riprova.", "error");
      return;
    }
    showToast("Salvato: " + autosaveLabel(el));
  });

  // "Città · Mario Rossi": the field's label and the row's name (Nome or
  // Descrizione, or the row title when that is empty).
  function autosaveLabel(el) {
    var label = el.getAttribute("aria-label") || "campo";
    var row = el.closest("tr");
    var who = "";
    if (row) {
      // the row's name: Nome (Clienti) or Descrizione (Prodotti)
      var name = row.querySelector("input[aria-label=Nome], input[aria-label=Descrizione]");
      who = name ? name.value.trim() : "";
      // names without letters/digits (e.g. ".") say nothing: use the row title (WhatsApp id)
      if (!/[\p{L}\p{N}]/u.test(who)) who = ((row.querySelector(".title-cell") || {}).textContent || "").trim();
    }
    return who ? label + " · " + who : label;
  }

  // Client-side filter: <input data-filter=".row-selector"> hides rows whose
  // data-name doesn't contain the typed text.
  document.addEventListener("input", function (e) {
    var sel = e.target.dataset && e.target.dataset.filter;
    if (!sel) return;
    var q = e.target.value.trim().toLowerCase();
    var scope = e.target.closest("main") || document;
    scope.querySelectorAll(sel).forEach(function (row) {
      var name = (row.dataset.name || row.textContent).toLowerCase();
      row.hidden = q !== "" && name.indexOf(q) === -1;
    });
    // groups (e.g. a month) disappear when none of their rows match
    scope.querySelectorAll("[data-filter-group]").forEach(function (g) {
      var items = g.querySelectorAll(g.dataset.filterGroup);
      g.hidden = items.length > 0 && Array.prototype.every.call(items, function (x) { return x.hidden; });
    });
  });

  // View toggle (e.g. Consegne: timeline / cards). The choice is remembered
  // in this browser; switching keeps the current selection.
  var VIEW_KEY = "adesgo-view-";
  function applyView(rootName, view) {
    var root = document.querySelector('[data-view-root="' + rootName + '"]');
    if (!root) return;
    root.classList.remove("view-timeline", "view-cards");
    root.classList.add("view-" + view);
    document.querySelectorAll("[data-view]").forEach(function (b) {
      b.classList.toggle("active", b.dataset.view === view);
      b.setAttribute("aria-pressed", b.dataset.view === view ? "true" : "false");
    });
  }
  function restoreViews() {
    // ?vista=schede|timeline in the URL wins over the remembered choice
    var fromURL = { schede: "cards", cards: "cards", timeline: "timeline" }[new URLSearchParams(location.search).get("vista")];
    document.querySelectorAll("[data-view-root]").forEach(function (root) {
      var v = fromURL || null;
      if (!v) {
        try { v = localStorage.getItem(VIEW_KEY + root.dataset.viewRoot); } catch (_) {}
      }
      if (v) applyView(root.dataset.viewRoot, v);
    });
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-view]");
    var root = document.querySelector("[data-view-root]");
    if (!b || !root) return;
    applyView(root.dataset.viewRoot, b.dataset.view);
    try { localStorage.setItem(VIEW_KEY + root.dataset.viewRoot, b.dataset.view); } catch (_) {}
  });

  // "Seleziona mese": ticks every visible checkbox of the section (or clears
  // them if all are already ticked), then notifies the form once.
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-check-all]");
    if (!b) return;
    var section = b.closest("section") || b.parentElement;
    var boxes = Array.prototype.filter.call(section.querySelectorAll("input[type=checkbox]"), function (x) {
      return !x.closest("[hidden]");
    });
    if (!boxes.length) return;
    var allOn = boxes.every(function (x) { return x.checked; });
    boxes.forEach(function (x) { x.checked = !allOn; });
    b.textContent = allOn ? "Seleziona mese" : "Deseleziona mese";
    boxes[0].dispatchEvent(new Event("change", { bubbles: true }));
  });

  // Clickable table rows (data-href).
  document.addEventListener("click", function (e) {
    var tr = e.target.closest("tr[data-href]");
    if (tr && !e.target.closest("a, button, input, select")) {
      window.location.href = tr.dataset.href;
    }
  });

  // "+ Aggiungi" for editable tables: clones the table's <template> row.
  // __L__ becomes the row letter (A, B, …), __N__ the row number (1, 2, …), __i__ the row index.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-add-row]");
    if (!btn) return;
    var table = document.querySelector(btn.dataset.addRow);
    var tpl = table && table.querySelector("template");
    if (!tpl) return;
    var tbody = table.tBodies[0];
    var n = tbody.querySelectorAll("tr").length;
    var max = parseInt(btn.dataset.max || "0", 10);
    if (max && n >= max) { showToast("Massimo " + max + " righe.", "warning"); return; }
    var html = tpl.innerHTML.replace(/__L__/g, "ABCDEFGHIJ".charAt(n)).replace(/__N__/g, String(n + 1)).replace(/__i__/g, String(n));
    tpl.insertAdjacentHTML("beforebegin", html);
    labelTables();
    var rows = tbody.querySelectorAll("tr");
    var input = rows[rows.length - 1].querySelector("input:not([type=hidden])");
    if (input) input.focus();
    if (max && n + 1 >= max) btn.disabled = true;
  });
})();
