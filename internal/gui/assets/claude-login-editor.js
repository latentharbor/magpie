// This mode configures Desktop's native egress proxy, not its provider.
async function showClaudeLoginEditor() {
  let targets = null, offered = [];
  const editor = el("div", "editor claude-login");
  const head = el("div", "ehead");
  head.append(icon("claude-color"), el("b", "", t("Keep Claude signed in")));
  const explanation = el("p", "hint", t("Keeps the official app and login. Only exact model matches are routed; all other requests stay official. Restart Claude Desktop after enabling or disabling."));
  const certificate = el("div", "hint login-certificate");
  const message = el("p", "hint"); message.setAttribute("role", "status");
  const validation = el("p", "hint", t("Code and Cowork compatibility needs a real test. Native UI does not prove backend features are available."));
  const fields = el("div", "login-fields");
  const address = input("127.0.0.1:3426", "127.0.0.1:3426"); address.setAttribute("aria-label", t("Proxy address"));
  const [enabledRow, enabled] = tick(t("Enable selected model routing"), false);
  fields.append(...field(t("Proxy address"), address), ...field(t("Status"), enabledRow));
  const [usageRow, usage] = tick(t("Let Magpie manage Code usage"), false);
  fields.append(...field(t("Code usage"), usageRow));
  const usageHint = el("p", "hint", t("Shows local Code quota as available. Every Code model needs a Magpie mapping. Real usage and rotation stay in Magpie; an official subscription is still required."));
  const organization = input("", "00000000-0000-0000-0000-000000000000"); organization.setAttribute("aria-label", t("Usage organization UUID"));
  fields.append(...field(t("Usage organization UUID"), organization));
  const usageCertificate = el("div", "hint login-usage-certificate");
  const prepareUsage = el("button", "text", t("Prepare quota certificate"));
  const mappings = el("div", "login-mappings");
  const add = el("button", "text", t("Add model mapping"));
  const mapProvider = el("button", "text", t("Map Claude models from a provider"));
  const prepare = el("button", "text", t("Prepare local certificate"));
  const refresh = el("button", "text", t("Refresh"));
  const close = el("button", "text", t("Cancel"));
  const save = el("button", "text primary", t("Save"));
  const bar = el("div", "bar"); bar.append(prepare, refresh, prepareUsage, el("span", "grow"), close, save);
  editor.append(head, explanation, fields, certificate, validation, usageHint, usageCertificate, mappings, add, mapProvider, message, bar);
  openModal(editor);
  const addMapping = (from = "", target = "") => {
    const row = el("div", "login-mapping");
    const source = input(from, t("Exact Claude model ID")); source.setAttribute("aria-label", t("Exact Claude model ID"));
    const destination = input(target, "provider/model"); destination.setAttribute("aria-label", t("Magpie model or routing group"));
    const pick = el("button", "text login-target-pick", t("Choose model"));
    const warning = el("p", "hint login-target-error"); warning.setAttribute("role", "status");
    const validate = () => {
      warning.textContent = targets && destination.value.trim() && !targets.some(x => x.v === destination.value.trim())
        ? t("This target is no longer available. Choose a model from an existing provider or routing group.") : "";
      warning.hidden = !warning.textContent;
    };
    destination.oninput = validate;
    pick.onclick = () => {
      if (targets) openProtoMenu(pick, targets, destination.value, value => { destination.value = value; validate(); }, "Choose model", "sess-menu", "right", false, "Search models…");
    };
    const remove = el("button", "text login-remove", t("Remove")); remove.type = "button"; remove.onclick = () => row.remove();
    row.append(source, el("span", "", "→"), destination, pick, remove, warning); mappings.append(row); validate();
  };
  const draw = (view, keepDraft = false) => {
    if (!keepDraft) { address.value = view.config.listen; enabled.checked = view.config.enabled; usage.checked = !!view.config.gatewayUsage; organization.value = view.config.usageOrganization || ""; }
    enabled.disabled = !view.certificate.trusted || !view.supported;
    certificate.replaceChildren(el("p", "", t(view.certificate.trusted ? "Local certificate is trusted" : "Local certificate needs your trust before enabling")), el("code", "", view.certificate.path));
    const quotaCA = view.usageCertificate;
    usageCertificate.replaceChildren(el("p", "", t("Organization usage needs a separate claude.ai certificate. Prepare it here, then explicitly trust its public CA. Subscription and login checks stay official.")));
    if (quotaCA) {
      usageCertificate.append(el("code", "", quotaCA.path));
      if (quotaCA.sha256) usageCertificate.append(el("p", "", "SHA-256: " + quotaCA.sha256));
      usageCertificate.append(el("p", "", t(quotaCA.trusted ? "Local certificate is trusted" : "Local certificate needs your trust before enabling")));
    }
    if (view.certificate.sha256) certificate.append(el("p", "", "SHA-256: " + view.certificate.sha256));
    certificate.append(el("p", "", t("Trust only this installation's public CA in Keychain Access. The private key stays on this computer. No certificate validation is disabled.")));
    if (!keepDraft) {
      mappings.replaceChildren(); for (const [from, target] of Object.entries(view.config.models || {})) addMapping(from, target);
      if (!mappings.children.length) addMapping();
    }
    message.textContent = view.runtime.error || (view.runtime.running ? t("Local inference proxy is running") : t("Local inference proxy is not running"));
  };
  add.onclick = () => addMapping(); close.onclick = () => closeModal();
  const readTargets = async () => {
    const list = await api("groups");
    offered = list.models || [];
    targets = [...(list.models || []).map(m => ({ v:m.id, name:m.name || m.id, note:m.providerName || "", literalName:true })),
      ...(list.groups || []).filter(g => g.ready).map(g => ({ v:g.ref || "group/" + g.id, name:g.name || g.id, note:t("Routing group"), literalName:true }))];
    for (const row of mappings.children) row.querySelectorAll("input")[1].oninput();
  };
  mapProvider.onclick = () => {
    const choices = [...new Map(offered.filter(m => m.id.startsWith(m.provider + "/claude-")).map(m => [m.provider, { v:m.provider, name:m.providerName || m.provider, note:"", literalName:true }])).values()];
    openProtoMenu(mapProvider, choices, "", provider => {
      for (const m of offered.filter(m => m.provider === provider && m.id.startsWith(provider + "/claude-"))) {
        const source = m.id.slice(provider.length + 1);
        const existing = [...mappings.children].find(row => row.querySelector("input").value.trim() === source);
        if (existing) {
          const input = existing.querySelectorAll("input")[1]; input.value = m.id; input.oninput();
        } else addMapping(source, m.id);
      }
      for (const row of [...mappings.children]) if ([...row.querySelectorAll("input")].every(input => !input.value.trim())) row.remove();
    }, "Map Claude models from a provider", "sess-menu");
  };
  refresh.onclick = async () => { try { await readTargets(); draw(await api("claude-login"), true); } catch (e) { message.textContent = e.message; } };
  prepare.onclick = async () => {
    prepare.disabled = true;
    try { draw(await api("claude-login/prepare", {}), true); } catch (e) { message.textContent = e.message; }
    finally { prepare.disabled = false; }
  };
  prepareUsage.onclick = async () => {
    prepareUsage.disabled = true;
    try { draw(await api("claude-login/prepare-usage", {}), true); } catch (e) { message.textContent = e.message; }
    finally { prepareUsage.disabled = false; }
  };
  save.onclick = async () => {
    const models = {};
    for (const row of mappings.children) {
      const [source, target] = row.querySelectorAll("input");
      if (!source.value.trim() && !target.value.trim()) continue;
      if (!source.value.trim() || !target.value.trim() || Object.hasOwn(models, source.value.trim())) {
        message.textContent = t("Each mapping needs a unique exact model ID and a Magpie target"); return;
      }
      models[source.value.trim()] = target.value.trim();
    }
    if (enabled.checked && (!Object.keys(models).length || targets && Object.values(models).some(target => !targets.some(x => x.v === target)))) {
      message.textContent = t("Choose at least one available model before enabling routing"); return;
    }
    save.disabled = true;
    try {
      draw(await api("claude-login", { enabled: enabled.checked, gatewayUsage: usage.checked, usageOrganization: organization.value.trim(), listen: address.value.trim(), models }));
      message.textContent = t("Saved — quit and reopen Claude Desktop to see it");
    } catch (e) { message.textContent = e.message; }
    finally { save.disabled = false; }
  };
  try { draw(await api("claude-login")); } catch (e) { message.textContent = e.message; save.disabled = true; }
  try { await readTargets(); } catch (e) { message.textContent = e.message; }
}
