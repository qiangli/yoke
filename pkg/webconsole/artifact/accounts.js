// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  let state = {
    accounts: [],
    principal: "",
    expandedID: null,
    details: {},
    usage: {},
    activePeriod: {},
    resources: {},
  };

  function isSafeURL(url) {
    if (!url || typeof url !== "string") return false;
    try {
      const u = new URL(url);
      return u.protocol === "http:" || u.protocol === "https:";
    } catch (_) {
      return false;
    }
  }

  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined && text !== null) node.textContent = text;
    return node;
  }

  function escapeHTML(s) {
    if (!s) return "";
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#039;");
  }

  async function loadAccounts() {
    const loading = $("acc-loading");
    const errBox = $("acc-error");
    const staleBox = $("acc-stale");
    const page = $("acc-page");

    if (loading) loading.style.display = "flex";
    if (errBox) errBox.style.display = "none";
    if (staleBox) staleBox.style.display = "none";
    if (page) page.setAttribute("aria-busy", "true");

    try {
      const res = await fetch("api/accounts");
      if (!res.ok) {
        throw new Error("HTTP " + res.status + " " + res.statusText);
      }
      const data = await res.json();
      state.accounts = data.accounts || [];
      state.principal = data.principal || "";

      if ($("acc-who") && state.principal) {
        $("acc-who").textContent = state.principal;
      }

      populateFilterOptions();
      renderTable();

      // Check for stale state or errors
      const staleCount = state.accounts.filter(a => a.is_stale || (a.last_error && a.last_error !== "")).length;
      if (staleCount > 0 && staleBox) {
        staleBox.textContent = staleCount + " account(s) have stale or unverified refresh state. Run refresh to update.";
        staleBox.style.display = "block";
      }
    } catch (err) {
      if (errBox) {
        errBox.textContent = "Could not load accounts: " + (err.message || err);
        errBox.style.display = "block";
      }
      if ($("acc-table")) $("acc-table").style.display = "none";
      if ($("acc-empty")) $("acc-empty").style.display = "none";
    } finally {
      if (loading) loading.style.display = "none";
      if (page) page.removeAttribute("aria-busy");
    }
  }

  function populateFilterOptions() {
    const providers = new Set();
    const categories = new Set();
    const teams = new Set();

    for (const a of state.accounts) {
      if (a.provider) providers.add(a.provider);
      if (a.category) categories.add(a.category);
      if (a.team) teams.add(a.team);
    }

    fillSelect("acc-filter-provider", Array.from(providers).sort());
    fillSelect("acc-filter-category", Array.from(categories).sort());
    fillSelect("acc-filter-team", Array.from(teams).sort());
  }

  function fillSelect(id, values) {
    const sel = $(id);
    if (!sel) return;
    const current = sel.value;
    while (sel.options.length > 1) {
      sel.remove(1);
    }
    for (const v of values) {
      const opt = document.createElement("option");
      opt.value = v;
      opt.textContent = v;
      sel.appendChild(opt);
    }
    if (values.includes(current)) {
      sel.value = current;
    }
  }

  function getFilteredAccounts() {
    const search = ($("acc-search") ? $("acc-search").value.trim().toLowerCase() : "");
    const provider = ($("acc-filter-provider") ? $("acc-filter-provider").value.toLowerCase() : "");
    const category = ($("acc-filter-category") ? $("acc-filter-category").value.toLowerCase() : "");
    const team = ($("acc-filter-team") ? $("acc-filter-team").value.toLowerCase() : "");
    const status = ($("acc-filter-status") ? $("acc-filter-status").value.toLowerCase() : "");

    return state.accounts.filter(a => {
      if (provider && (a.provider || "").toLowerCase() !== provider) return false;
      if (category && (a.category || "").toLowerCase() !== category) return false;
      if (team && (a.team || "").toLowerCase() !== team) return false;
      if (status && (a.status || "").toLowerCase() !== status) return false;
      if (search) {
        const hay = [a.alias, a.name, a.provider, a.team, a.username, a.id].filter(Boolean).join(" ").toLowerCase();
        if (!hay.includes(search)) return false;
      }
      return true;
    });
  }

  function renderTable() {
    const tbody = $("acc-tbody");
    const table = $("acc-table");
    const emptyBox = $("acc-empty");
    if (!tbody || !table || !emptyBox) return;

    tbody.replaceChildren();
    const list = getFilteredAccounts();

    if (list.length === 0) {
      table.style.display = "none";
      emptyBox.style.display = "block";
      return;
    }

    table.style.display = "table";
    emptyBox.style.display = "none";

    for (const a of list) {
      const tr = el("tr", "acc-row");
      tr.setAttribute("data-id", a.id);

      // Avatar / initials
      const tdAvatar = el("td");
      const avatarSpan = el("span", "acc-avatar");
      if (a.avatar_url && isSafeURL(a.avatar_url)) {
        const img = document.createElement("img");
        img.src = a.avatar_url;
        img.alt = a.alias || a.name || "avatar";
        avatarSpan.appendChild(img);
      } else {
        avatarSpan.textContent = a.initials || "AC";
      }
      tdAvatar.appendChild(avatarSpan);
      tr.appendChild(tdAvatar);

      // Alias
      const tdAlias = el("td");
      const aliasSpan = el("span", "acc-alias", a.alias || a.name || a.id);
      aliasSpan.style.fontWeight = "600";
      tdAlias.appendChild(aliasSpan);
      if (a.username) {
        const sub = el("div", null, a.username);
        sub.style.fontSize = "0.78rem";
        sub.style.color = "var(--muted-foreground)";
        tdAlias.appendChild(sub);
      }
      tr.appendChild(tdAlias);

      // Provider / Category
      const tdProv = el("td");
      tdProv.appendChild(el("span", "acc-provider", a.provider || ""));
      const catSub = el("div", "acc-category", a.category || "");
      catSub.style.fontSize = "0.78rem";
      catSub.style.color = "var(--muted-foreground)";
      tdProv.appendChild(catSub);
      tr.appendChild(tdProv);

      // Team / Profile
      const tdTeam = el("td");
      tdTeam.appendChild(el("span", "acc-team", a.team || a.profile || "—"));
      if (a.team && a.profile && a.team !== a.profile) {
        const profSub = el("div", "acc-profile", a.profile);
        profSub.style.fontSize = "0.78rem";
        profSub.style.color = "var(--muted-foreground)";
        tdTeam.appendChild(profSub);
      }
      tr.appendChild(tdTeam);

      // Status
      const tdStatus = el("td");
      const st = a.status || "unresolved";
      const badge = el("span", "acc-badge acc-status-" + st, st);
      tdStatus.appendChild(badge);
      tr.appendChild(tdStatus);

      // Quota / Reset (Render as unavailable, never 0)
      const tdQuota = el("td");
      const quotaVal = (a.quota && a.quota !== "") ? a.quota : "unavailable";
      const qSpan = el("span", "acc-quota", quotaVal);
      tdQuota.appendChild(qSpan);
      if (a.reset && a.reset !== "" && a.reset !== "unavailable") {
        const rSub = el("div", "acc-reset", "reset: " + a.reset);
        rSub.style.fontSize = "0.78rem";
        rSub.style.color = "var(--muted-foreground)";
        tdQuota.appendChild(rSub);
      }
      tr.appendChild(tdQuota);

      // Spend with currency (Render as unavailable, never 0)
      const tdSpend = el("td");
      const spendVal = (a.spend && a.spend !== "") ? a.spend : "unavailable";
      const sSpan = el("span", "acc-spend", spendVal);
      tdSpend.appendChild(sSpan);
      tr.appendChild(tdSpend);

      // Freshness age
      const tdFresh = el("td");
      const fSpan = el("span", "acc-freshness", a.freshness_age || "never");
      tdFresh.appendChild(fSpan);
      if (a.last_error) {
        const errTag = el("div", "acc-stale-tag", a.last_error);
        errTag.style.fontSize = "0.75rem";
        errTag.style.color = "#dc2626";
        tdFresh.appendChild(errTag);
      }
      tr.appendChild(tdFresh);

      // Actions (Expand toggle & Refresh)
      const tdActions = el("td");
      const expandBtn = el("button", "acc-btn acc-expand-btn", state.expandedID === a.id ? "Hide" : "Details");
      expandBtn.type = "button";
      expandBtn.addEventListener("click", (e) => {
        e.stopPropagation();
        toggleDetail(a.id);
      });
      tdActions.appendChild(expandBtn);

      const refBtn = el("button", "acc-btn acc-refresh-btn", "Refresh");
      refBtn.type = "button";
      refBtn.style.marginLeft = "0.4rem";
      refBtn.setAttribute("data-id", a.id);
      refBtn.addEventListener("click", async (e) => {
        e.stopPropagation();
        await refreshAccount(a.id, refBtn);
      });
      tdActions.appendChild(refBtn);
      tr.appendChild(tdActions);

      tr.addEventListener("click", () => toggleDetail(a.id));
      tbody.appendChild(tr);

      // Detail row if expanded
      if (state.expandedID === a.id) {
        const detailTr = el("tr", "acc-detail-row");
        const detailTd = el("td");
        detailTd.colSpan = 9;
        detailTd.style.padding = "0";
        detailTd.appendChild(renderDetailPane(a));
        detailTr.appendChild(detailTd);
        tbody.appendChild(detailTr);
      }
    }
  }

  function toggleDetail(id) {
    if (state.expandedID === id) {
      state.expandedID = null;
    } else {
      state.expandedID = id;
      loadDetail(id);
    }
    renderTable();
  }

  async function loadDetail(id) {
    if (state.details[id]) return;
    try {
      const res = await fetch("api/accounts/" + encodeURIComponent(id));
      if (!res.ok) throw new Error("detail HTTP " + res.status);
      const data = await res.json();
      state.details[id] = data;

      // Also load resources and usage for this account
      loadResources(id);
      loadUsage(id, state.activePeriod[id] || "month");
    } catch (e) {
      state.details[id] = { error: String(e.message || e) };
      renderTable();
    }
  }

  async function loadResources(id) {
    try {
      const res = await fetch("api/accounts/" + encodeURIComponent(id) + "/resources");
      if (res.ok) {
        const data = await res.json();
        state.resources[id] = data.resources || [];
        renderTable();
      }
    } catch (_) {}
  }

  async function loadUsage(id, period) {
    state.activePeriod[id] = period;
    try {
      const res = await fetch("api/accounts/" + encodeURIComponent(id) + "/usage?period=" + encodeURIComponent(period));
      if (res.ok) {
        const data = await res.json();
        if (!state.usage[id]) state.usage[id] = {};
        state.usage[id][period] = data.metrics || [];
        renderTable();
      }
    } catch (_) {}
  }

  function renderDetailPane(summary) {
    const pane = el("div", "acc-detail-pane");
    pane.id = "acc-detail-" + summary.id;
    const detail = state.details[summary.id];

    if (!detail) {
      const loading = el("div", "acc-loading-box", "Loading details...");
      pane.appendChild(loading);
      return pane;
    }
    if (detail.error) {
      pane.appendChild(el("div", "acc-alert", "Error loading detail: " + detail.error));
      return pane;
    }

    const a = detail.account || {};
    const safeLinks = detail.safe_links || {};
    const grid = el("div", "acc-detail-grid");

    // Card 1: Identity & URLs
    const cardIdentity = el("div", "acc-detail-card");
    cardIdentity.appendChild(el("h4", null, "Account Identity"));
    
    const idRow = el("div", null, "ID: ");
    const idCode = el("code", "acc-detail-id", a.id || summary.id);
    idRow.appendChild(idCode);
    cardIdentity.appendChild(idRow);

    if (a.provider_account_id) {
      cardIdentity.appendChild(el("div", null, "Provider Account ID: " + a.provider_account_id));
    }
    if (a.email) {
      cardIdentity.appendChild(el("div", null, "Email: " + a.email));
    }
    if (a.region) {
      cardIdentity.appendChild(el("div", null, "Region: " + a.region));
    }
    if (a.environment) {
      cardIdentity.appendChild(el("div", null, "Environment: " + a.environment));
    }

    // Safe links: rel=noopener with http(s) scheme check
    const linksDiv = el("div");
    linksDiv.style.marginTop = "0.5rem";
    if (safeLinks.profile_url && isSafeURL(safeLinks.profile_url)) {
      const l = el("a", "acc-safe-link", "Profile");
      l.href = safeLinks.profile_url;
      l.target = "_blank";
      l.rel = "noopener";
      linksDiv.appendChild(l);
      linksDiv.appendChild(document.createTextNode(" "));
    }
    if (safeLinks.dashboard_url && isSafeURL(safeLinks.dashboard_url)) {
      const l = el("a", "acc-safe-link", "Dashboard");
      l.href = safeLinks.dashboard_url;
      l.target = "_blank";
      l.rel = "noopener";
      linksDiv.appendChild(l);
    }
    if (linksDiv.childNodes.length > 0) {
      cardIdentity.appendChild(linksDiv);
    }
    grid.appendChild(cardIdentity);

    // Card 2: Credential References only (never secrets)
    const cardCreds = el("div", "acc-detail-card acc-detail-creds");
    cardCreds.appendChild(el("h4", null, "Credential References & Scopes"));
    const conns = a.connections || [];
    if (conns.length === 0) {
      cardCreds.appendChild(el("div", null, "No connections recorded."));
    } else {
      for (const c of conns) {
        const cDiv = el("div");
        cDiv.style.marginBottom = "0.4rem";
        cDiv.appendChild(el("div", null, "Ref: " + (c.credential_ref || "none")));
        if (c.scopes && c.scopes.length > 0) {
          cDiv.appendChild(el("div", null, "Scopes: " + c.scopes.join(", ")));
        }
        if (c.auth_method) {
          cDiv.appendChild(el("div", null, "Auth: " + c.auth_method));
        }
        cardCreds.appendChild(cDiv);
      }
    }
    grid.appendChild(cardCreds);

    // Card 3: CLI Commands
    const cardCLI = el("div", "acc-detail-card");
    cardCLI.appendChild(el("h4", null, "Copyable CLI Examples"));
    const examples = detail.cli_examples || [
      "bashy account show " + summary.id,
      "bashy account usage " + summary.id,
      "bashy account refresh " + summary.id,
    ];
    for (const cmd of examples) {
      const box = el("div", "acc-cli-box");
      const c = el("code", "acc-cli-example", cmd);
      box.appendChild(c);
      const copyBtn = el("button", "acc-btn acc-copy-btn", "Copy");
      copyBtn.type = "button";
      copyBtn.addEventListener("click", () => {
        navigator.clipboard.writeText(cmd).then(() => {
          copyBtn.textContent = "Copied!";
          setTimeout(() => { copyBtn.textContent = "Copy"; }, 1500);
        });
      });
      box.appendChild(copyBtn);
      cardCLI.appendChild(box);
    }
    grid.appendChild(cardCLI);
    pane.appendChild(grid);

    // Resources Section
    const resCard = el("div", "acc-detail-card acc-detail-resources");
    resCard.style.marginBottom = "1rem";
    resCard.appendChild(el("h4", null, "Discovered Resources"));
    const resources = state.resources[summary.id] || (detail.snapshot && detail.snapshot.resources) || a.resources || [];
    if (resources.length === 0) {
      resCard.appendChild(el("div", null, "No resources cached. Refresh to inspect provider resources."));
    } else {
      const resTable = el("table", "acc-resource-table");
      resTable.innerHTML = "<thead><tr><th>Kind</th><th>Name / ID</th><th>Source</th></tr></thead><tbody></tbody>";
      const rbody = resTable.querySelector("tbody");
      for (const r of resources) {
        const row = el("tr");
        row.appendChild(el("td", null, r.kind || "—"));
        row.appendChild(el("td", null, r.name ? (r.name + " (" + r.id + ")") : r.id));
        row.appendChild(el("td", null, r.source || "—"));
        rbody.appendChild(row);
      }
      resCard.appendChild(resTable);
    }
    pane.appendChild(resCard);

    // Usage Section
    const usageCard = el("div", "acc-detail-card acc-detail-usage");
    usageCard.appendChild(el("h4", null, "Usage History & Billing"));

    const periodTabs = el("div", "acc-period-tabs");
    const activeP = state.activePeriod[summary.id] || "month";
    for (const p of ["day", "week", "month"]) {
      const tab = el("button", "acc-period-btn" + (activeP === p ? " active" : ""), p);
      tab.type = "button";
      tab.addEventListener("click", () => loadUsage(summary.id, p));
      periodTabs.appendChild(tab);
    }
    usageCard.appendChild(periodTabs);

    const metricsMap = state.usage[summary.id] || (detail.snapshot && detail.snapshot.metrics) || {};
    const metrics = metricsMap[activeP] || [];
    if (metrics.length === 0) {
      usageCard.appendChild(el("div", null, "No metrics recorded for period: " + activeP + "."));
    } else {
      const metricTable = el("table", "acc-metric-table");
      metricTable.innerHTML = "<thead><tr><th>Metric</th><th>Value</th><th>Unit</th><th>Classification</th><th>Coverage</th></tr></thead><tbody></tbody>";
      const mbody = metricTable.querySelector("tbody");
      for (const m of metrics) {
        const row = el("tr");
        row.appendChild(el("td", null, m.name || "—"));

        // Value renders "unavailable", never 0
        let valStr = "unavailable";
        if (m.value !== null && m.value !== undefined) {
          if (m.currency && m.currency !== "") {
            valStr = m.value.toFixed(2) + " " + m.currency;
          } else if (m.unit && m.unit.toLowerCase() === "usd") {
            valStr = "$" + m.value.toFixed(2);
          } else {
            valStr = String(m.value);
          }
        }
        row.appendChild(el("td", "acc-metric-value", valStr));
        row.appendChild(el("td", null, m.unit || "—"));
        row.appendChild(el("td", "acc-metric-classification", m.classification || "—"));
        row.appendChild(el("td", "acc-metric-coverage", m.coverage || "—"));
        mbody.appendChild(row);
      }
      usageCard.appendChild(metricTable);
    }
    pane.appendChild(usageCard);

    return pane;
  }

  async function refreshAccount(id, btn) {
    const origText = btn.textContent;
    btn.disabled = true;
    btn.textContent = "Refreshing...";
    const errBox = $("acc-error");
    if (errBox) errBox.style.display = "none";

    try {
      const res = await fetch("api/accounts/refresh", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: id }),
      });
      if (!res.ok) {
        const errData = await res.json().catch(() => ({}));
        throw new Error(errData.error || ("HTTP " + res.status));
      }
      delete state.details[id];
      delete state.usage[id];
      delete state.resources[id];
      await loadAccounts();
      if (state.expandedID === id) {
        await loadDetail(id);
      }
    } catch (e) {
      if (errBox) {
        errBox.textContent = "Refresh failed for " + id + ": " + (e.message || e);
        errBox.style.display = "block";
      }
    } finally {
      btn.disabled = false;
      btn.textContent = origText;
    }
  }

  async function refreshAll() {
    const btn = $("acc-refresh-all");
    if (!btn) return;
    const origText = btn.textContent;
    btn.disabled = true;
    btn.textContent = "Refreshing all...";
    const errBox = $("acc-error");
    if (errBox) errBox.style.display = "none";

    try {
      const res = await fetch("api/accounts/refresh", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ all: true }),
      });
      if (!res.ok) {
        const errData = await res.json().catch(() => ({}));
        throw new Error(errData.error || ("HTTP " + res.status));
      }
      state.details = {};
      state.usage = {};
      state.resources = {};
      await loadAccounts();
    } catch (e) {
      if (errBox) {
        errBox.textContent = "Refresh all failed: " + (e.message || e);
        errBox.style.display = "block";
      }
    } finally {
      btn.disabled = false;
      btn.textContent = origText;
    }
  }

  // Event handlers
  function setupEvents() {
    const search = $("acc-search");
    if (search) {
      search.addEventListener("input", renderTable);
    }
    for (const filterID of ["acc-filter-provider", "acc-filter-category", "acc-filter-team", "acc-filter-status"]) {
      const el = $(filterID);
      if (el) {
        el.addEventListener("change", renderTable);
      }
    }
    const refAll = $("acc-refresh-all");
    if (refAll) {
      refAll.addEventListener("click", refreshAll);
    }
  }

  // Boot
  document.addEventListener("DOMContentLoaded", () => {
    setupEvents();
    loadAccounts();
  });
  if (document.readyState !== "loading") {
    setupEvents();
    loadAccounts();
  }
})();
