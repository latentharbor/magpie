// A subscription's transport is chosen for each new request. Changing this
// preference never stops or switches one that is already running.
function claudeRepliesRows(s, keep) {
  // The CLI prompt preference stays saved while the HTTP mode hides it.
  const lean = el("div", "row pref");
  lean.id = "claudeLeanRow";
  lean.hidden = s.claudeTransport === "http";
  const lwho = el("div", "who");
  lwho.append(el("div", "name", t("Short Claude system prompt")),
    el("div", "sub wraps", t("A Claude subscription answers with the agent's own instructions as its system prompt, in place of Claude Code's, about 1K fewer tokens each request. Still the real Claude Code, but its requests look less like Claude Code's own, which can put the account more at risk")));
  const lval = el("div", "val");
  lval.append(segs([["off", t("Off")], ["on", t("On")]], s.claudeLeanPrompt ? "on" : "off",
    (v) => savePrefs({ ...keep, claudeLeanPrompt: v === "on" })));
  lean.append(lwho, lval);
  const row = el("div", "row pref rule-row");
  row.id = "claudeTransportRow";
  const who = el("div", "who");
  who.append(el("div", "name", t("Claude subscription transport")),
    el("div", "sub wraps", t("HTTP is experimental and impersonates Claude Code. It may put your account at risk of restriction or suspension. Equivalent subscription quota usage is not guaranteed. Changes apply to new requests; requests already running keep their transport")));
  const choices = [{ v: "cli", name: "Claude Code CLI (claude -p)" }, { v: "http", name: "Claude Code HTTP (experimental)" }];
  const chosen = s.claudeTransport || "cli";
  const pick = el("button", "proto pick");
  pick.type = "button";
  pick.id = "claudeTransportPick";
  pick.dataset.value = chosen;
  pick.setAttribute("aria-label", t("Claude subscription transport"));
  pick.setAttribute("aria-haspopup", "menu");
  pick.setAttribute("aria-expanded", "false");
  pick.append(el("span", "", t(choices.find((x) => x.v === chosen)?.name || chosen)), svg(CHEV, 11, 1.6));
  pick.onclick = (e) => {
    e.stopPropagation();
    if (pick.classList.contains("open")) return closeProtoMenu();
    openProtoMenu(pick, choices, chosen, (v) => {
      if (v !== chosen) savePrefs({ ...keep, claudeTransport: v });
    }, "Claude subscription transport", "sess-menu", "right");
  };
  row.append(who, pick);
  return [lean, row];
}
