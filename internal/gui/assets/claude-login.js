let claudeLoginEditor;
document.getElementById("claudeLoginOpen").onclick = async () => {
  try {
    await (claudeLoginEditor ||= new Promise((resolve, reject) => {
      const s = document.createElement("script");
      s.src = "claude-login-editor.js";
      s.onload = resolve;
      s.onerror = () => { s.remove(); claudeLoginEditor = null; reject(Error(t("Could not load settings"))); };
      document.head.append(s);
    }));
    await showClaudeLoginEditor();
  } catch (e) { status(e.message, "err"); }
};
