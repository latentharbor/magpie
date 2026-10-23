// Claude transport is opt-in, translated, kept by other settings saves,
// and chosen with the app's menu. All requests use isolated API fixtures.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { watchSettingsReads, settingsDrawn } = require("./reader.cjs");
const assets = process.env.MAGPIE_TRANSPORT_ASSETS || path.resolve(__dirname, "../assets");
const words = {
  en: ["Claude subscription transport", "Claude Code HTTP (experimental)", "restriction or suspension", "not guaranteed", "requests already running"],
  zh: ["Claude 订阅传输方式", "Claude Code HTTP（实验性）", "受限或封禁", "不保证", "正在运行的请求"],
  "zh-TW": ["Claude 訂閱傳輸方式", "Claude Code HTTP（實驗性）", "受限或停權", "不保證", "執行中的請求"],
  ja: ["Claude サブスクリプションの通信方式", "Claude Code HTTP（実験的）", "制限または停止", "保証はありません", "実行中のリクエスト"],
  de: ["Claude-Abo: Übertragung", "Claude Code HTTP (experimentell)", "eingeschränkt oder gesperrt", "nicht garantiert", "laufende Anfragen"],
};

function serve(lang, settings, saved) {
  return async (r) => {
    const url = new URL(r.request().url());
    const json = (data) => r.fulfill({ json: data });
    if (url.pathname === "/boot.js") return r.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return r.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings });
    if (url.pathname === "/api/settings") {
      if (r.request().method() === "POST") {
        saved.push(JSON.parse(r.request().postData()));
        Object.assign(settings, saved.at(-1));
      }
      return json(settings);
    }
    if (url.pathname === "/api/groups") return json({ models: [], groups: [], pools: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true, url: "http://127.0.0.1:3999" } });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await r.fulfill({ body: await fs.readFile(file), contentType });
  };
}

async function reveal(page, row, width) {
  await page.mouse.move(width / 2, 450);
  for (let i = 0; i < 60; i++) {
    const box = await row.boundingBox();
    if (box && box.y >= 0 && box.y + box.height <= 900) return;
    await page.mouse.wheel(0, box && box.y < 0 ? -120 : 120);
    await page.waitForTimeout(30);
  }
  assert.fail("transport row could not be reached");
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of Object.keys(words)) for (const width of [1100, 420]) {
    test(`${engine} ${lang} ${width}: Claude transport is opt-in and saved`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      const page = await (await browser.newContext({ viewport: { width, height: 900 } })).newPage();
      page.setDefaultTimeout(5000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      const saved = [];
      const settings = { lang, theme: "light", redact: true, claudeLeanPrompt: true };
      await page.route("**/*", serve(lang, settings, saved));
      await watchSettingsReads(page);
      await page.goto("http://magpie.test/?view=settings&tab=models");
      await settingsDrawn(page);
      const row = page.locator("#replyList #claudeTransportRow");
      await row.waitFor();
      await reveal(page, row, width);
      assert.equal(await row.locator(".name").textContent(), words[lang][0]);
      const sub = await row.locator(".sub").textContent();
      for (const warning of words[lang].slice(2)) assert(sub.includes(warning), sub);
      assert.equal(await page.locator("#claudeTransportPick").textContent(), "Claude Code CLI (claude -p)");
      assert.equal(await page.locator("#claudeTransportPick").getAttribute("data-value"), "cli");
      assert.equal(await page.locator("#claudeLeanRow").isVisible(), true);
      assert.equal(await row.locator("select").count(), 0);
      for (const part of [".name", ".sub", ".pick"]) {
        assert(await row.locator(part).evaluate((e) => {
          const r = e.getBoundingClientRect();
          return e.scrollWidth <= e.clientWidth + 1 && r.width > 0 && r.right <= innerWidth + 1;
        }), `${part} fits at ${width}`);
      }
      const before = await page.evaluate(() => [scrollX, scrollY, document.scrollingElement.scrollTop]);
      await page.locator("#claudeTransportPick").click();
      const http = page.getByRole("menuitemradio", { name: words[lang][1], exact: true });
      await http.click();
      await page.waitForFunction(() => document.querySelector("#claudeTransportPick")?.dataset.value === "http");
      assert.equal(saved.length, 1);
      assert.equal(saved[0].claudeTransport, "http");
      assert.equal(saved[0].redact, true);
      assert.equal(saved[0].claudeLeanPrompt, true, "CLI choice kept while hidden");
      assert.equal(await page.locator("#claudeLeanRow").isVisible(), false);
      assert.deepEqual(await page.evaluate(() => [scrollX, scrollY, document.scrollingElement.scrollTop]), before, "click does not scroll");
      await page.reload();
      await settingsDrawn(page);
      assert.equal(await page.locator("#claudeTransportPick").getAttribute("data-value"), "http");
      // A neighboring preference save keeps HTTP selected.
      await reveal(page, page.locator("#loopGuardRow"), width);
      await page.locator("#loopGuardRow .segs button").first().click();
      await page.waitForFunction(() => document.querySelector("#loopGuardRow .segs button")?.getAttribute("aria-pressed") === "true");
      assert.equal(saved.at(-1).claudeTransport, "http");
      await reveal(page, row, width);
      await page.locator("#claudeTransportPick").click();
      await page.getByRole("menuitemradio", { name: "Claude Code CLI (claude -p)", exact: true }).click();
      await page.waitForFunction(() => document.querySelector("#claudeTransportPick")?.dataset.value === "cli");
      assert.equal(saved.at(-1).claudeTransport, "cli");
      assert.equal(await page.locator("#claudeLeanRow").isVisible(), true);
      assert.deepEqual(errors, []);
    });
  }
}
