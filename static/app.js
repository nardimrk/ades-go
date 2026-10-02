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

  // Mobile menu arrows: show "<" / ">" where more sections are hidden.
  function updateNavArrows() {
    var wrap = document.querySelector("[data-nav-wrap]");
    var nav = wrap && wrap.querySelector("nav");
    if (!nav) return;
    var max = nav.scrollWidth - nav.clientWidth;
    wrap.classList.toggle("can-left", max > 1 && nav.scrollLeft > 1);
    wrap.classList.toggle("can-right", max > 1 && nav.scrollLeft < max - 1);
  }
  document.addEventListener("scroll", function (e) {
    if (e.target.closest && e.target.closest("[data-nav-wrap]")) updateNavArrows();
  }, true);
  window.addEventListener("resize", updateNavArrows);
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-nav-scroll]");
    if (!b) return;
    var nav = b.parentElement.querySelector("nav");
    nav.scrollBy({ left: Number(b.dataset.navScroll) * nav.clientWidth * 0.7, behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
  });

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

  // Suggestions for <input list="…">: the native <datalist> popup is hidden
  // or unusable on phones (iOS shows only a few items above the keyboard), so
  // we draw our own list under the field, filled from the same <datalist>.
  // Matches every typed word anywhere in the option, ignoring case and accents.
  var COMBO_MAX = 50;
  var combo = null; // { input, list, items, active }

  function fold(s) {
    return s.normalize("NFD").replace(/[̀-ͯ]/g, "").toLowerCase();
  }

  function initCombos(root) {
    root.querySelectorAll("input[list]").forEach(function (inp) {
      inp.dataset.combo = inp.getAttribute("list");
      inp.removeAttribute("list");
      inp.setAttribute("autocomplete", "off");
      inp.setAttribute("role", "combobox");
      inp.setAttribute("aria-autocomplete", "list");
      inp.setAttribute("aria-expanded", "false");
    });
  }

  function closeCombo() {
    if (!combo) return;
    combo.list.remove();
    combo.input.setAttribute("aria-expanded", "false");
    combo = null;
  }

  function openCombo(inp) {
    var dl = document.getElementById(inp.dataset.combo);
    if (!dl) return;
    if (!combo || combo.input !== inp) {
      closeCombo();
      var list = document.createElement("ul");
      list.className = "combo-list";
      list.setAttribute("role", "listbox");
      inp.parentNode.classList.add("combo-host");
      inp.insertAdjacentElement("afterend", list);
      combo = { input: inp, list: list, items: [], active: -1 };
      inp.setAttribute("aria-expanded", "true");
    }
    var words = fold(inp.value.trim()).split(/\s+/).filter(Boolean);
    var matches = [], total = 0;
    Array.prototype.forEach.call(dl.options, function (o) {
      var hay = fold(o.value + " " + o.textContent);
      if (!words.every(function (w) { return hay.indexOf(w) !== -1; })) return;
      total++;
      if (matches.length < COMBO_MAX) matches.push(o);
    });
    combo.items = matches;
    combo.active = -1;
    combo.list.innerHTML = "";
    matches.forEach(function (o, i) {
      var li = document.createElement("li");
      li.setAttribute("role", "option");
      li.dataset.i = i;
      li.textContent = o.value;
      var extra = o.textContent.trim();
      if (extra && extra !== o.value) {
        var s = document.createElement("small");
        s.textContent = extra;
        li.appendChild(s);
      }
      combo.list.appendChild(li);
    });
    var note = "";
    if (!total) note = "Nessun risultato";
    else if (total > matches.length) note = matches.length + " di " + total + ": continua a scrivere per restringere";
    if (note) {
      var li = document.createElement("li");
      li.className = "combo-note";
      li.textContent = note;
      combo.list.appendChild(li);
    }
  }

  function pickCombo(i) {
    var o = combo && combo.items[i];
    if (!o) return;
    var inp = combo.input;
    inp.value = o.value;
    closeCombo();
    inp.dispatchEvent(new Event("input", { bubbles: true }));
    inp.dispatchEvent(new Event("change", { bubbles: true }));
  }

  function moveCombo(d) {
    var n = combo.items.length;
    if (!n) return;
    combo.active = (combo.active + d + n) % n;
    Array.prototype.forEach.call(combo.list.querySelectorAll("li[data-i]"), function (li, i) {
      li.classList.toggle("active", i === combo.active);
      if (i === combo.active) li.scrollIntoView({ block: "nearest" });
    });
  }

  document.addEventListener("focusin", function (e) {
    if (e.target.dataset && e.target.dataset.combo) openCombo(e.target);
    else if (combo && !combo.list.contains(e.target)) closeCombo();
  });
  document.addEventListener("input", function (e) {
    if (e.target.dataset && e.target.dataset.combo) openCombo(e.target);
  });
  document.addEventListener("keydown", function (e) {
    if (!combo || e.target !== combo.input) return;
    if (e.key === "ArrowDown") { e.preventDefault(); moveCombo(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); moveCombo(-1); }
    else if (e.key === "Escape") closeCombo();
    else if (e.key === "Enter" && combo.active >= 0) { e.preventDefault(); pickCombo(combo.active); }
  });
  // mousedown keeps the focus in the field (the list stays open while tapping)
  document.addEventListener("mousedown", function (e) {
    if (combo && combo.list.contains(e.target)) e.preventDefault();
  });
  document.addEventListener("click", function (e) {
    if (!combo) return;
    var li = e.target.closest && e.target.closest(".combo-list li[data-i]");
    if (li) pickCombo(Number(li.dataset.i));
    else if (e.target !== combo.input) closeCombo();
  });
  document.addEventListener("htmx:beforeSwap", closeCombo);

  function init(root) {
    initCombos(root);
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

  document.addEventListener("DOMContentLoaded", function () { init(document); revealActiveNav(); updateNavArrows(); });
  document.addEventListener("htmx:load", function (e) { init(e.target); revealActiveNav(); updateNavArrows(); });

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
  // data-name doesn't contain the typed text. Column filters
  // (<th><input data-col-filter></th>) also hide rows whose cell in that
  // column (its input's value, or its text) doesn't contain theirs.
  function matchesColumns(row) {
    var t = row.closest("table");
    if (!t) return true;
    return Array.prototype.every.call(t.querySelectorAll("thead [data-col-filter]"), function (f) {
      var q = f.value.trim().toLowerCase();
      if (!q) return true;
      var cell = row.children[f.closest("th").cellIndex];
      if (!cell) return false;
      var inp = cell.querySelector("input:not([type=hidden])");
      return (inp ? inp.value : cell.textContent).toLowerCase().indexOf(q) !== -1;
    });
  }

  function runFilters(scope) {
    scope.querySelectorAll("[data-filter]").forEach(function (box) {
      var q = box.value.trim().toLowerCase();
      scope.querySelectorAll(box.dataset.filter).forEach(function (row) {
        var name = (row.dataset.name || row.textContent).toLowerCase();
        row.hidden = (q !== "" && name.indexOf(q) === -1) || !matchesColumns(row);
      });
    });
    // "12 di 282 clienti" while filtering, "282 clienti" otherwise
    scope.querySelectorAll("[data-filter-count]").forEach(function (c) {
      var rows = scope.querySelectorAll(c.dataset.filterCount);
      var shown = Array.prototype.filter.call(rows, function (r) { return !r.hidden; }).length;
      c.textContent = (shown === rows.length ? "" : shown + " di ") + rows.length + " " + c.dataset.noun;
    });
    // groups (e.g. a month) disappear when none of their rows match
    scope.querySelectorAll("[data-filter-group]").forEach(function (g) {
      var items = g.querySelectorAll(g.dataset.filterGroup);
      g.hidden = items.length > 0 && Array.prototype.every.call(items, function (x) { return x.hidden; });
    });
  }

  document.addEventListener("input", function (e) {
    var d = e.target.dataset;
    if (!d || !(d.filter || d.colFilter !== undefined)) return;
    runFilters(e.target.closest("main") || document);
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
