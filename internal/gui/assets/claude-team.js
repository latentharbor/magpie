// Each organization is an independent subscription using the normal router.
function claudeTeamRoutingHint(routing) {
  return routing === "" ? t("Among accounts with quota, uses the weekly reset first, then the 5-hour reset. No preferred account.") : "";
}

function claudeTeamSummary(p) {
  return t("Seats in use: {used}/{total}", {used:p.team.inUse,total:p.team.seats});
}

function claudeTeamTile() {
  const b = pickRow("claude-color", "Claude Team");
  b.onclick = () => addClaudeTeam();
  return b;
}

async function addClaudeTeam() {
  const ed = el("div", "editor claude-team team-create"), head = el("div", "ehead");
  head.append(icon("claude-color"), el("b", "", "Claude Team"));
  const key = input("", "sessionKey", "password"); key.autocomplete = "off";
  const org = input("", t("Organization UUID"));
  const fields = el("div", "team-fields");
  fields.append(...field(t("Administrator sessionKey"), key));
  const organizations = el("div", "team-organizations"), message = el("p", "hint team-message");
  message.setAttribute("role", "status");
  const cancel = el("button", "text", t("Cancel")), next = el("button", "text primary", t("Continue"));
  const bar = el("div", "bar"); bar.append(el("span", "grow"), cancel, next);
  ed.append(head, el("p", "hint team-intro", t("Add the administrator session first. Configure rotation and add accounts in the provider afterwards.")), fields, organizations, message, bar);
  openModal(ed);
  let busy = false, manual = false, selected = "";
  const create = async id => {
    const v = await api("claude-team/create", { sessionKey:key.value.trim(), organizationId:id });
    key.value = "";
    adding = false; draft = null; editing = "claude-team-" + v.config.organizationId;
    await closeModal();
    await loadProvidersNow();
  };
  cancel.onclick = () => { key.value = ""; closeModal(); };
  next.onclick = async () => {
    if (busy || !key.value.trim()) return;
    busy = true; next.disabled = true; message.textContent = t("Loading…");
    try {
      if (manual || selected) await create(manual ? org.value.trim() : selected);
      else {
        const choices = await api("claude-team/discover", { sessionKey:key.value.trim() });
        if (choices.length === 1) await create(choices[0].id);
        else {
          organizations.replaceChildren();
          for (const c of choices) {
            const [row, box] = tick((c.name || "Claude Team") + " · " + c.id, false);
            box.type = "radio"; box.name = "team-organization"; box.onchange = () => { selected = c.id; };
            organizations.append(row);
          }
          message.textContent = t("Choose the organization to add");
        }
      }
    } catch (e) {
      message.textContent = e.message;
      // Discovery may be unavailable while a known organization's members API
      // works. Keep its UUID as an explicit alternative, never guess one.
      if (!manual) { manual = true; fields.append(...field(t("Organization UUID"), org)); }
    } finally { busy = false; next.disabled = false; }
  };
}

function claudeTeamSignedInProvider(st) {
  return st.agent === "claude" && providers.providers.find((p) => p.team?.members?.some((m) => m.selected && m.user === st.user));
}

async function addClaudeTeamAccount(organizationId, event) {
  scrollOnPurpose(event, 3000);
  editing = null; draft = null; adding = false;
  await closeModal();
  editing = "claude-team-" + organizationId;
  await startSignIn("claude", false, undefined, { organizationId });
}

function renderTeamAccounts(a, p) {
  const list = el("div", "accts team-accts");
  drawTeamAccounts(list, p);
  const poll = async () => {
    if (!list.isConnected) return;
    try {
      const team = await api("claude-team/members?organizationId=" + encodeURIComponent(p.teamOrganization));
      if (!list.isConnected) return;
      if (JSON.stringify(team) !== JSON.stringify(p.team)) {
        p = {...p, team}; drawTeamAccounts(list, p);
      }
    } catch { /* Keep the last snapshot; a failed read is not an empty pool. */ }
    if (list.isConnected) setTimeout(poll, p.team?.members?.some(m => m.quotaPending) ? 1500 : 10000);
  };
  setTimeout(poll, p.team?.members?.some(m => m.quotaPending) ? 1500 : 10000);
  return list;
}

function drawTeamAccounts(list, p) {
  // Polls update member rows without removing the sign-in's focused controls.
  const flow = list.querySelector(":scope > .signing");
  for (const child of [...list.children]) if (child !== flow) child.remove();
  const rows = document.createDocumentFragment();
  const settings = el("button", "text", "Claude Team");
  settings.onclick = async () => { editing = null; draft = null; await closeModal(); openClaudeTeam(p.teamOrganization); };
  rows.append(settings);
  if (p.team) rows.append(el("div", "hint", t("Seats in use: {used}/{total}", { used: p.team.inUse, total: p.team.seats })));
  rows.append(el("p", "hint", t("Cached usage; refreshed when a seat is assigned")));
  for (const m of (p.team?.members || []).filter((m) => m.selected)) {
    const row = el("div", "acc with-aq"), dot = el("span", "dot tick fixed");
    if (m.on) dot.append(svg(CHECK, 10, 2.2));
    const state = m.seatTier === "team_tier_1" ? "Premium" : t("Unassigned");
    row.append(dot, el("span", "rename", m.user), el("span", "grow"), el("span", "plan", state));
    const q = { windows: m.windows || (m.limits || []).map((w) => ({ name: w.kind === "five_hour" ? "5 hours" : w.kind === "seven_day" ? "7 days" : w.kind,
      used: w.used * 100, resetsAt: w.resetsAt ? new Date(w.resetsAt * 1000).toISOString() : null })), readDeferred: true, asOf: m.observedAt };
    const quota = accountQuota({ [m.user]: q }, m.user);
    if (m.quotaError || m.quotaPending) quota.append(el("span", "aq-asof", t(m.quotaPending ? "Collecting usage…" : m.quotaError)));
    row.append(quota);
    rows.append(row);
  }
  if (signing?.agent === "claude" && signing.organizationId === p.teamOrganization) {
    if (!flow) list.prepend(renderSigning(subOf("claude")));
    list.append(rows);
  }
  else {
    flow?.remove();
    const add = el("button", "acc add team-add-account");
    const dot = el("span", "dot"); dot.append(svg(PLUS, 10, 1.8));
    add.append(dot, el("span", "n", t("Add another {name} account", { name: "Claude" })));
    add.onclick = (event) => addClaudeTeamAccount(p.teamOrganization, event);
    rows.append(add);
    list.append(rows);
  }
}

async function openClaudeTeam(organizationId = "") {
  if (!organizationId) return addClaudeTeam();
  const ed = el("div", "editor claude-team"), head = el("div", "ehead");
  head.append(icon("claude-color"), el("b", "", "Claude Team"));
  const message = el("div", "hint team-message"), panel = el("div", "team-summary");
  message.setAttribute("role", "status");
  const name = input("", t("Name")), org = input(organizationId, t("Organization UUID"));
  const key = input("", "sessionKey", "password"), threshold = input("100", "100");
  key.autocomplete = "off"; org.spellcheck = false; threshold.inputMode = "decimal";
  org.readOnly = !!organizationId;
  const [enableRow, enabled] = tick(t("Enable seat rotation"), false);
  const fields = el("div", "team-fields"), chooseOrg = el("div", "team-organizations");
  let routing = "";
  const routeBox = el("div", "team-routing");
  const drawRouting = () => {
    routeBox.replaceChildren(routingSegs(ROUTINGS.filter(([id]) => id !== "weight").map(([id, label]) => [id, t(label)]), routing, (value) => { routing = value; drawRouting(); }));
    const hint = claudeTeamRoutingHint(routing) || t(ROUTINGS.find(([id]) => id === routing)?.[2] || "");
    if (hint) routeBox.append(el("p", "hint", hint));
  };
  fields.append(...field(t("Name"), name), ...field(t("Organization UUID"), org),
    ...field(t("Administrator sessionKey"), key, t("Stored on this computer only; leave blank to keep the saved credential")),
    ...field(t("Rotate at usage (%)"), threshold), ...field(t("Routing"), routeBox), ...field(t("Status"), enableRow));
  const pool = el("div", "team-pool"), bar = el("div", "bar");
  const cancel = el("button", "text", t("Cancel")), save = el("button", "text primary", t("Save")), refresh = el("button", "text", t("Refresh"));
  cancel.onclick = () => { key.value = ""; closeModal(); };
  bar.append(refresh, el("span", "grow"), cancel, save);
  ed.append(head, el("p", "hint team-intro", t("Premium accounts are on; standby accounts stay off. Existing replies finish before a seat moves.")), fields, chooseOrg, panel, message, pool, bar);
  openModal(ed);
  let view, selected = new Set(), changed = new Set(), pending = false, timer;
  const date = (x) => new Date(x).toLocaleString(intlLang());
  const fail = (e) => { message.textContent = e.message; };
  const draw = () => {
    panel.replaceChildren(); pool.replaceChildren();
    if (!view) return;
    panel.append(el("b", "", t("Seats in use: {used}/{total}", { used: view.inUse || 0, total: view.seats || 0 })));
    if (view.error) panel.append(el("p", "hint", view.error));
    if (view.nextReset) panel.append(el("p", "hint", t("Earliest reset: {time}", { time: date(view.nextReset) })));
    pool.append(el("h3", "", t("Seat pool")));
    const members = (view.members || []).filter((m) => m.organizationId.toLowerCase() === org.value.trim().toLowerCase());
    if (!members.length) pool.append(el("p", "hint", t("Sign in to at least two Claude accounts in this organization first")));
    for (const m of members) {
      const row = el("div", "team-member"), label = el("label", "team-check"), box = el("input");
      row.dataset.account = m.accountId; box.type = "checkbox"; box.checked = selected.has(m.accountId);
      box.onchange = () => { changed.add(m.accountId); box.checked ? selected.add(m.accountId) : selected.delete(m.accountId); };
      const user = el("span", "team-user", m.user); user.title = m.user;
      label.append(box, user);
      const top = el("div", "team-member-head"); top.append(label);
      const state = m.seatTier === "team_tier_1" ? "Premium" : m.seatTier === "unassigned" ? t("Unassigned") : t("Not verified");
      top.append(el("span", "team-seat", state)); row.append(top);
      const windows = el("div", "team-windows");
      for (const kind of ["five_hour", "seven_day"]) {
        const name = kind === "five_hour" ? "5 hours" : "7 days";
        const old = (m.limits || []).find((w) => w.kind === kind);
        const w = m.windows ? m.windows.find(w => w.name === name) : old && {used:old.used * 100, resetsAt:old.resetsAt && new Date(old.resetsAt * 1000).toISOString()};
        const line = el("div", "team-window"), label = t(name);
        if (w) {
          const meter = el("progress"); meter.max = 100; meter.value = w.used; meter.setAttribute("aria-label", label);
          line.append(el("span", "", label + " · " + Math.round(w.used) + "%"), meter,
            el("span", "hint", w.resetsAt ? t("Resets: {time}", { time: date(w.resetsAt) }) : kind === "five_hour" && w.used === 0 ? t("Window not started") : t("Reset time unknown")));
        } else line.append(el("span", "", label), el("span", "hint", t("Usage unknown")));
        windows.append(line);
      }
      row.append(windows);
      const states = { signed_out: "Sign in again", cooling: "Cooling down", unknown: "Usage unknown", ready: "Ready" };
      const foot = el("div", "team-member-foot");
      if (m.windows?.some(w => w.estimated)) foot.append(el("span", "hint", t("Reset-based estimate; not refreshed from Claude")));
      if (states[m.state]) foot.append(el("span", "hint", t(states[m.state])));
      if (m.observedAt) foot.append(el("span", "hint", t("Last observed: {time}", { time: date(m.observedAt) })));
      if (m.quotaError && !m.quotaPending) foot.append(el("span", "hint", t(m.quotaError)));
      if (m.quotaPending) foot.append(el("span", "hint", t("Collecting usage…")));
      if (m.selected) {
        const retry = el("button", "text", t("Retry usage collection"));
        retry.disabled = pending || m.quotaPending;
        retry.onclick = async () => {
          if (pending) return;
          pending = true; draw();
          try { view = await api("claude-team/quota", { organizationId: view.config.organizationId, accountId: m.accountId }); }
          catch (e) { fail(e); }
          finally { pending = false; draw(); poll(); }
        };
        foot.append(retry);
      }
      if (m.seatTier === "unassigned" && m.limits?.length) foot.append(el("span", "hint", t("Cached usage; refreshed when a seat is assigned")));
      if (m.seatTier === "team_tier_1") {
        const rotate = el("button", "text", t("Rotate seat now")); rotate.disabled = !view.config.enabled || view.rotating || pending;
        rotate.onclick = async () => {
          if (pending) return;
          pending = true; draw();
          try { view = await api("claude-team/rotate", { organizationId: view.config.organizationId, from: m.accountId }); message.textContent = t("Seat rotation requested"); }
          catch (e) { fail(e); }
          finally { pending = false; draw(); poll(); }
        };
        foot.append(rotate);
      }
      row.append(foot); pool.append(row);
    }
    if (organizationId) {
      const add = el("button", "text team-add-account", t("Add another {name} account", { name: "Claude" }));
      add.disabled = pending;
      add.onclick = (event) => addClaudeTeamAccount(organizationId, event);
      pool.append(add);
    }
    if (view.history?.length) {
      const history = el("details", "team-history"); history.append(el("summary", "", t("Recent seat changes")));
      const who = (id) => view.members.find((m) => m.accountId === id)?.user || id;
      const result = { assigned: "Assigned", rolled_back: "Rolled back", unchanged: "Unchanged" };
      for (const h of view.history.slice(-5).reverse()) history.append(el("p", "hint", date(h.at) + " · " + who(h.from) + " → " + who(h.to) + " · " + t(result[h.result] || "Not verified")));
      pool.append(history);
    }
  };
  const load = async (initial = false) => {
    try {
      const next = await api("claude-team/members?" + (organizationId ? "organizationId=" + encodeURIComponent(organizationId) : "new=1"));
      if (!ed.isConnected) return;
      if (!initial && JSON.stringify(next) === JSON.stringify(view)) { poll(); return; }
      view = next;
      for (const id of view.config.members || []) if (!changed.has(id)) selected.add(id);
      if (initial) {
        name.value = view.config.name || ""; org.value = view.config.organizationId || organizationId;
        threshold.value = view.config.threshold || 100; enabled.checked = !!view.config.enabled;
        routing = providers?.providers.find((p) => p.teamOrganization === organizationId)?.routing || view.config.routing || ""; drawRouting();
        key.placeholder = view.sessionSet ? t("Saved; leave blank to keep") : "sessionKey";
        selected = new Set(view.config.members || []);
        if (!organizationId) {
          const ids = [...new Set(view.members.map((m) => m.organizationId))];
          for (const id of ids) {
            const b = el("button", "text", id); b.type = "button";
            b.onclick = () => { org.value = id; selected = new Set(view.members.filter((m) => m.organizationId === id).map((m) => m.accountId)); draw(); };
            chooseOrg.append(b);
          }
        }
      }
      draw(); poll();
    } catch (e) { fail(e); }
  };
  const poll = () => {
    clearTimeout(timer);
    if (ed.isConnected) timer = setTimeout(() => { if (ed.isConnected && !$("#modal").hidden) load(); },
      view?.rotating || view?.members?.some((m) => m.quotaPending) ? 1500 : 30000);
  };
  org.oninput = draw;
  refresh.onclick = () => load();
  save.onclick = async () => {
    if (pending) return;
    const value = decimalOf(threshold.value);
    if (!Number.isFinite(value) || value < 1 || value > 100) { threshold.focus({ preventScroll: true }); return; }
    pending = true; save.disabled = true;
    try {
      view = await api("claude-team/config", { name: name.value.trim(), routing, enabled: enabled.checked, organizationId: org.value.trim(), sessionKey: key.value,
        threshold: value, members: [...selected].filter((id) => (view.members || []).some((m) => m.accountId === id && m.organizationId.toLowerCase() === org.value.trim().toLowerCase())) });
      key.value = "";
      changed.clear(); selected = new Set(view.config.members || []);
      organizationId = view.config.organizationId;
      if (view.members?.some((m) => m.selected && (m.quotaPending || m.quotaError))) { draw(); poll(); }
      else await closeModal();
      await loadProvidersNow();
    } catch (e) { fail(e); }
    finally {
      pending = false; save.disabled = false;
      if (ed.isConnected) { draw(); poll(); }
    }
  };
  save.disabled = true; await load(true); save.disabled = !view;
}
