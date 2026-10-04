// Small progressive enhancements on top of htmx.
(function () {
  "use strict";

  function showToast(msg, kind) {
    var box = document.getElementById("toasts");
    if (!box) return;
    var el = document.createElement("div");
    el.className = "toast toast-" + (kind || "success");
    el.dataset.autohide = "";
    el.dataset.bound = "1";
    el.textContent = msg;
    box.appendChild(el);
    autohide(el);
  }

  // Every toast (from the server or from showToast) gets a close button and
  // disappears by itself after a few seconds.
  var CLOSE_ICON = '<svg class="icon-svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.25" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"></path></svg>';
  function autohide(el) {
    if (!el.querySelector(".toast-close")) {
      var text = document.createElement("span");
      text.className = "toast-text";
      while (el.firstChild) text.appendChild(el.firstChild);
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "toast-close";
      btn.setAttribute("aria-label", "Chiudi notifica");
      btn.innerHTML = CLOSE_ICON;
      el.appendChild(text);
      el.appendChild(btn);
    }
    setTimeout(function () { el.classList.add("hide"); }, 3500);
    setTimeout(function () { el.remove(); }, 4000);
  }
  document.addEventListener("click", function (e) {
    var btn = e.target.closest(".toast-close");
    if (btn) btn.closest(".toast").remove();
  });

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
    // a customer / wine field can create the one that isn't there
    var newKind = inp.dataset.newCustomer ? "cliente" : inp.dataset.newProduct ? "prodotto" : "";
    if (!total && newKind && inp.value.trim()) {
      var add = document.createElement("li");
      add.className = "combo-new";
      add.setAttribute("role", "option");
      add.textContent = "+ Nuovo " + newKind + " «" + inp.value.trim() + "»";
      combo.list.appendChild(add);
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
  // "+ Nuovo cliente": the modal opens with the typed name
  var newCustomerFor = null;
  document.addEventListener("click", function (e) {
    var add = e.target.closest && e.target.closest(".combo-list li.combo-new");
    if (!add || !combo) return;
    var inp = combo.input;
    var dlg = document.getElementById(inp.dataset.newCustomer || inp.dataset.newProduct);
    closeCombo();
    if (!dlg) return;
    newCustomerFor = inp;
    var f = dlg.querySelector("form");
    if (f) f.reset();
    var first = dlg.querySelector("[name=name], [name=description]");
    first.value = inp.value.trim();
    dlg.showModal();
    var next = dlg.querySelector("[name=telefono], [name=winery]");
    if (next && !matchMedia("(pointer: coarse)").matches) next.focus();
  });
  // created: it joins the suggestions and becomes the field's value
  function onCreated(e) {
    var name = e.detail && e.detail.name;
    if (!name) return;
    var inp = newCustomerFor;
    if (inp) {
      var dl = document.getElementById(inp.dataset.combo);
      if (dl) { var o = document.createElement("option"); o.value = name; dl.appendChild(o); }
      inp.value = name;
      inp.dispatchEvent(new Event("change", { bubbles: true }));
    }
    var dlg = e.target.closest && e.target.closest("dialog");
    if (dlg && dlg.open) dlg.close();
    newCustomerFor = null;
  }
  document.body.addEventListener("clienteCreato", onCreated);
  document.body.addEventListener("prodottoCreato", onCreated);

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
    syncFlagView();
    openModals(root);
    if (root !== document) runFilters(document.querySelector("main") || document);
    root.querySelectorAll("[data-proposal]").forEach(proposalTotal);
    // a toast from the server is itself the htmx:load root
    var toasts = root.matches && root.matches("[data-autohide]") ? [root] : root.querySelectorAll("[data-autohide]");
    Array.prototype.forEach.call(toasts, function (el) {
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
    // the server saved it normalized ("pd" → "Padova"): show that
    var saved = xhr && xhr.getResponseHeader("X-Autosave-Value");
    if (saved !== null && saved !== undefined) el.value = decodeURIComponent(saved);
    showToast("Salvato: " + autosaveLabel(el));
    // a new value of a field with suggestions (Città) is suggested from now on
    var dl = el.list, v = el.value.trim();
    if (dl && v && !Array.prototype.some.call(dl.options, function (o) { return o.value.toLowerCase() === v.toLowerCase(); })) {
      var opt = document.createElement("option");
      opt.value = v;
      dl.appendChild(opt);
    }
    // the same field elsewhere in the row (Clienti: the table and the
    // phone's "Dettagli" dialog) shows the new value too
    var row = el.closest("tr");
    if (row) row.querySelectorAll("[data-autosave]").forEach(function (o) {
      if (o !== el && o.getAttribute("hx-post") === el.getAttribute("hx-post") && o.getAttribute("hx-vals") === el.getAttribute("hx-vals")) o.value = el.value;
    });
    var dlg = el.closest("dialog[data-reload-on-save]");
    if (dlg) dlg.dataset.saved = "1";
  });
  // a dialog whose fields change how the page is grouped (Consegne: the
  // address) reloads the page when it closes after a save
  document.addEventListener("close", function (e) {
    var dlg = e.target;
    if (dlg.matches && dlg.matches("dialog[data-reload-on-save][data-saved]")) location.reload();
  }, true);

  // "Città · Mario Rossi": the field's label and the row's name (Nome or
  // Descrizione, or the row title when that is empty).
  function autosaveLabel(el) {
    var label = el.getAttribute("aria-label") || "campo";
    var row = el.closest("tr");
    var who = "";
    var box = el.closest("[data-autosave-who]");
    if (box) return label + " · " + box.dataset.autosaveWho;
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
      // case and accents don't matter ("citta" finds "Città")
      var q = fold(box.value.trim());
      scope.querySelectorAll(box.dataset.filter).forEach(function (row) {
        var name = fold(row.dataset.name || row.textContent);
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

  // "Controlla risposte": the proposed-order editor. The -/+ buttons change
  // the quantity (empty = option not touched); the total follows.
  function proposalTotal(box) {
    var tot = 0, any = false;
    box.querySelectorAll("input[data-price], input[data-new-qty]").forEach(function (inp) {
      var n = parseInt(inp.value, 10);
      // a new option's price is the one typed next to it
      var price = inp.hasAttribute("data-new-qty")
        ? parseFloat((inp.closest("[data-new-option]").querySelector("[name=new_price]").value || "").replace(",", "."))
        : parseFloat(inp.dataset.price);
      if (!isNaN(n) && n > 0 && !isNaN(price)) { tot += n * price; any = true; }
    });
    var out = box.querySelector("[data-prop-total]");
    if (out) out.textContent = any ? "Totale € " + tot.toFixed(2) : "";
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-step]");
    if (!b) return;
    var inp = b.parentElement.querySelector("input");
    var n = parseInt(inp.value, 10);
    var step = Number(b.dataset.step);
    if (isNaN(n)) { if (step < 0) return; n = 0; }
    inp.value = String(Math.min(999, Math.max(0, n + step)));
    proposalTotal(b.closest("[data-proposal]"));
  });
  document.addEventListener("input", function (e) {
    var box = e.target.closest && e.target.closest("[data-proposal]");
    if (box) proposalTotal(box);
  });

  // "+ Aggiungi opzione" in the confirm modal: a new option row with the
  // first letter (A–J) the order doesn't use yet.
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-add-option]");
    if (!b) return;
    var box = b.closest("[data-proposal]");
    var tpl = box && box.querySelector("template[data-new-option-template]");
    if (!tpl) return;
    var used = {};
    box.querySelectorAll(".prop-opt b").forEach(function (x) { used[x.textContent.trim().replace(/^cassa\s+/i, "")] = true; });
    var letter = "ABCDEFGHIJ".split("").filter(function (l) { return !used[l]; })[0];
    if (!letter) { showToast("L'ordine ha già tutte le opzioni da A a J.", "warning"); return; }
    var wrap = document.createElement("div");
    wrap.innerHTML = tpl.innerHTML.replace(/__L__/g, letter);
    var row = wrap.firstElementChild;
    box.querySelector("[data-new-options]").appendChild(row);
    initCombos(row);
    var wine = row.querySelector("[name=new_wine]");
    if (wine && !matchMedia("(pointer: coarse)").matches) wine.focus();
  });
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-remove-option]");
    if (!b) return;
    var box = b.closest("[data-proposal]");
    b.closest("[data-new-option]").remove();
    if (box) proposalTotal(box);
  });

  // All replies / only the ones to check. The choice lives on [data-replies],
  // so it survives the out-of-band refresh of the filter buttons.
  function syncFlagView() {
    var root = document.querySelector("[data-replies]");
    if (!root) return;
    var v = root.dataset.show || "all";
    document.querySelectorAll("[data-flag-view]").forEach(function (b) {
      b.classList.toggle("active", b.dataset.flagView === v);
      b.setAttribute("aria-pressed", b.dataset.flagView === v ? "true" : "false");
    });
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-flag-view]");
    var root = document.querySelector("[data-replies]");
    if (!b || !root) return;
    root.dataset.show = b.dataset.flagView;
    syncFlagView();
  });

  // Modals (<dialog data-modal-open>, e.g. "Conferma come ordine"): opened
  // as soon as htmx inserts them, removed when closed (Esc, Annulla, X,
  // tap outside). The server empties #modal once the action succeeds.
  function openModals(root) {
    var list = root.matches && root.matches("dialog[data-modal-open]") ? [root] : root.querySelectorAll("dialog[data-modal-open]");
    Array.prototype.forEach.call(list, function (dlg) {
      if (dlg.open || !dlg.showModal) return;
      if (!dlg.hasAttribute("data-modal-keep")) dlg.addEventListener("close", function () { dlg.remove(); });
      dlg.showModal();
      var first = dlg.querySelector("input[type=number]");
      if (first && !matchMedia("(pointer: coarse)").matches) first.focus();
    });
  }
  document.addEventListener("click", function (e) {
    var dlg = e.target.closest("dialog.modal");
    if (!dlg) return;
    if (e.target === dlg || e.target.closest("[data-modal-close]")) dlg.close();
  });

  // Modals kept in the page (<dialog data-modal-keep>, e.g. "Modifica
  // cliente" inside a form): opened by a [data-modal-show="<id>"] button.
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-modal-show]");
    var dlg = b && document.getElementById(b.dataset.modalShow);
    if (!dlg || dlg.open || !dlg.showModal) return;
    dlg.showModal();
    var first = dlg.querySelector("input[type=text]");
    if (first && !matchMedia("(pointer: coarse)").matches) first.focus();
  });
  // Enter in a modal field submits with the modal's own button, not the
  // first submit button of the form around it (e.g. a row's delete).
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Enter" || e.defaultPrevented || e.target.tagName !== "INPUT") return;
    var dlg = e.target.closest("dialog.modal");
    var sub = dlg && dlg.querySelector(".modal-foot button[type=submit]");
    if (sub) { e.preventDefault(); sub.click(); }
  });

  // Floating button in the screen's corner. Mobile browsers (iOS Safari and
  // Firefox, with their bottom bar) can end the area that "bottom: 0" refers
  // to above the bottom of what is visible: a fixed probe measures that gap
  // and --fab-lift lowers the button by it (0 where they match: desktop,
  // Android). ?fabdebug=1 shows the numbers.
  var fabProbe = null;
  function placeFab() {
    var fab = document.querySelector("[data-fab]");
    if (!fab) return;
    if (!fabProbe) {
      fabProbe = document.createElement("div");
      fabProbe.setAttribute("aria-hidden", "true");
      fabProbe.style.cssText = "position:fixed;left:0;bottom:0;width:1px;height:1px;pointer-events:none;visibility:hidden";
      document.body.appendChild(fabProbe);
    }
    var vv = window.visualViewport;
    var visibleBottom = vv ? vv.offsetTop + vv.height : window.innerHeight;
    var fixedBottom = fabProbe.getBoundingClientRect().bottom;
    var gap = Math.max(0, Math.round(visibleBottom - fixedBottom));
    if (gap > 120) gap = 0; // not a toolbar strip (e.g. the keyboard is open)
    document.documentElement.style.setProperty("--fab-lift", gap + "px");
    if (/[?&]fabdebug=1/.test(location.search)) {
      var d = document.getElementById("fab-debug");
      if (!d) {
        d = document.createElement("div");
        d.id = "fab-debug";
        d.style.cssText = "position:fixed;top:0;left:0;z-index:99;background:#ff0;color:#000;font:12px monospace;padding:2px 4px";
        document.body.appendChild(d);
      }
      d.textContent = "inner " + window.innerHeight + " vv " + (vv ? Math.round(vv.offsetTop) + "+" + Math.round(vv.height) : "-") +
        " fixedBottom " + Math.round(fixedBottom) + " lift " + gap + " safe " + getComputedStyle(fab).getPropertyValue("--fab-safe");
    }
  }
  placeFab();
  window.addEventListener("resize", placeFab);
  window.addEventListener("scroll", placeFab, { passive: true });
  if (window.visualViewport) {
    window.visualViewport.addEventListener("resize", placeFab);
    window.visualViewport.addEventListener("scroll", placeFab);
  }
  document.addEventListener("htmx:afterSettle", placeFab);
  // Clienti, Prodotti: a new page of rows starts from the top of the table
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail.target;
    if (!t || !/^(clienti|prodotti)-body$/.test(t.id || "") || e.detail.requestConfig.verb !== "get") return;
    var body = document.getElementById(t.id);
    var wrap = body && body.closest(".table-wrap");
    if (wrap) wrap.scrollTop = 0;
  });

  // Floating sections button (phones/tablets): fans out the main sections.
  function setFab(fab, open) {
    fab.classList.toggle("open", open);
    var b = fab.querySelector("[data-fab-toggle]");
    if (b) b.setAttribute("aria-expanded", open ? "true" : "false");
  }
  document.addEventListener("click", function (e) {
    var fab = e.target.closest("[data-fab]");
    if (!fab) return;
    if (e.target.closest("[data-fab-toggle]")) setFab(fab, !fab.classList.contains("open"));
    else if (e.target.closest("[data-fab-close], .fab-item")) setFab(fab, false);
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    var fab = document.querySelector("[data-fab].open");
    if (fab) { setFab(fab, false); fab.querySelector("[data-fab-toggle]").focus(); }
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

  // Notifications bell: a click opens the frosted preview and fires
  // "notif-open" (htmx loads the unseen orders and the server marks them seen,
  // resetting the badge out-of-band). Outside click or Esc closes it.
  function notifParts() {
    return { btn: document.querySelector("[data-notif-btn]"), panel: document.getElementById("notif-panel") };
  }
  function closeNotif(focus) {
    var n = notifParts();
    if (!n.panel || n.panel.hidden) return;
    n.panel.hidden = true;
    n.btn.setAttribute("aria-expanded", "false");
    if (focus) n.btn.focus();
  }
  function openNotif() {
    var n = notifParts();
    // phones/tablets: the menu is a top bar, the panel drops down under it
    if (window.matchMedia("(max-width: 900px)").matches) {
      var bar = n.btn.closest(".sidebar") || n.btn;
      n.panel.style.setProperty("--notif-top", Math.round(bar.getBoundingClientRect().bottom + 8) + "px");
    }
    n.panel.innerHTML = '<p class="notif-loading muted">Caricamento…</p>';
    n.panel.hidden = false;
    n.panel.classList.remove("in");
    void n.panel.offsetWidth; // restart the entrance animation
    n.panel.classList.add("in");
    n.btn.setAttribute("aria-expanded", "true");
    htmx.trigger(n.btn, "notif-open");
  }
  document.addEventListener("click", function (e) {
    var n = notifParts();
    if (!n.panel) return;
    if (e.target.closest("[data-notif-btn]")) {
      if (n.panel.hidden) openNotif(); else closeNotif(false);
      return;
    }
    if (!n.panel.hidden && !e.target.closest("#notif-panel")) closeNotif(false);
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") closeNotif(true);
  });
  // badge: pops in when the number grows; the bell rings when orders arrive
  // while the page is open
  var notifLast = -1;
  document.addEventListener("htmx:load", function (e) {
    var b = e.target;
    if (!b || b.id !== "notif-count") return;
    if ((b.getAttribute("hx-trigger") || "").indexOf("load") === 0) return; // placeholder of a new page
    var c = parseInt(b.dataset.count || "0", 10);
    if (c > 0 && c > notifLast) {
      b.classList.add("pop");
      var btn = b.closest("[data-notif-btn]");
      if (btn && notifLast >= 0) {
        btn.classList.remove("ring");
        void btn.offsetWidth;
        btn.classList.add("ring");
      }
    }
    var btn2 = b.closest("[data-notif-btn]");
    if (btn2) btn2.setAttribute("aria-label", c > 0 ? "Nuovi ordini: " + c : "Nuovi ordini");
    notifLast = c;
  });
})();
