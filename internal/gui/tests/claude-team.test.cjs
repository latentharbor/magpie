// Real Chromium/WebKit rendering against in-memory APIs, never live seats.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { strings } = require("./strings.cjs");
const assets = process.env.MAGPIE_TEAM_ASSETS || path.resolve(__dirname, "../assets");
const id = n => "00000000-0000-4000-8000-" + String(n).padStart(12, "0");
const org = id(1), second = id(70);
function fixture(lang) {
  const posts = [], views = new Map(), providers = [];
  for (const [organizationId, count, offset] of [[org, 13, 2], [second, 2, 71]]) {
    const members = Array.from({length:count}, (_, i) => ({
      accountId:id(offset+i), organizationId, user: (i === 1 ? "long-account-name-for-wrapping-" : "account-") + (offset+i) + "@example.test",
      selected:true, on:i===0, seatTier:i===0?"team_tier_1":"unassigned", state:i===0?"premium":"ready",
      observedAt:"2026-10-23T10:00:00Z", limits:[
        {kind:"five_hour",used:.23,resetsAt:4102444800},
        {kind:"seven_day",used:i===2?1:.67,resetsAt:4102790400},
      ],
    }));
    views.set(organizationId, {config:{name:organizationId===org?"Glac Team":"Second Team",enabled:true,organizationId,members:members.map(m=>m.accountId),threshold:100,routing:""},
      sessionSet:true,seats:1,inUse:1,running:true,rotating:false,history:[],members});
  }
  function provider(v) {
    return {id:"claude-team-"+v.config.organizationId,name:v.config.name,icon:"claude-color",chat:"https://example.invalid/v1",models:[],agents:[],fallback:[],headers:{},
      key:{set:true,masked:"fixture"},routing:v.config.routing,teamOrganization:v.config.organizationId,team:v,
      account:{agent:"claude",agentName:"Claude Code",agentIcon:"claude-color",user:v.members[0].user,plan:"Team",
        logins:v.members.map(m=>({agent:"claude",user:m.user,on:m.on,active:false,plan:"Team"}))}};
  }
  providers.push(provider(views.get(org)));
  const f = {posts,views,providers};
  f.route = async route => {
    const req=route.request(), url=new URL(req.url()), json=data=>route.fulfill({json:data});
    if(url.pathname==="/boot.js") return route.fulfill({contentType:"text/javascript",body:"window.bootPrefs="+JSON.stringify({lang,theme:"light",web:true})+";"});
    if(url.pathname==="/wails/runtime.js") return route.fulfill({contentType:"text/javascript",body:"export const Window={};"});
    if(url.pathname==="/api/state") return json({agents:[],profiles:[],settings:{lang,theme:"light"}});
    if(url.pathname==="/api/providers") return json({providers,presets:[],excluded:[],gateway:{running:true,window:true}});
    if(url.pathname==="/api/groups") return json({groups:[]});
    if(url.pathname.startsWith("/api/claude-team/")) {
      if(req.method()==="POST") {
        const body=req.postDataJSON(); posts.push({path:url.pathname,body});
        const v=views.get(body.organizationId);
        if(url.pathname.endsWith("config")) {
          v.config={...body}; delete v.config.sessionKey;
          if (f.failCaptureOnSave) v.members[1].quotaError="Quota collection failed; retry while this account has Premium access";
          let index=providers.findIndex(p=>p.teamOrganization===body.organizationId);
          if(index<0) providers.push(provider(v)); else providers[index]=provider(v);
        } else if(url.pathname.endsWith("quota")) {
          const m=v.members.find(m=>m.accountId===body.accountId);
          delete m.quotaError; m.quotaPending=true;
        } else v.history.push({at:"2026-10-23T10:00:00Z",from:id(2),to:id(3),result:"assigned"});
        return json(v);
      }
      if(url.searchParams.get("new")==="1") return json({config:{enabled:false,members:[],threshold:100},members:[...views.values()].flatMap(v=>v.members),history:[]});
      return json(views.get(url.searchParams.get("organizationId")||org));
    }
    if(url.pathname.startsWith("/api/")) return json({});
    const file=path.join(assets,url.pathname==="/"?"index.html":url.pathname);
    try { return route.fulfill({body:await fs.readFile(file),contentType:{".html":"text/html",".js":"text/javascript",".css":"text/css",".svg":"image/svg+xml"}[path.extname(file)]}); }
    catch { return route.fulfill({status:404,body:""}); }
  };
  return f;
}
for(const engine of (process.env.BROWSER?[process.env.BROWSER]:["chromium","webkit"])) {
  for(const lang of (process.env.LANG_ONLY?[process.env.LANG_ONLY]:["en","zh","zh-TW","ja","de"])) {
    test(engine+" "+lang+": normal subscriptions, cached quotas, multi-org forms and no preferred account", {timeout:60000}, async t=>{
      const browser=await(engine==="webkit"?webkit.launch():chromium.launch({channel:"chromium"}));
      t.after(()=>browser.close());
      const page=await(await browser.newContext({viewport:{width:1000,height:850},reducedMotion:"reduce"})).newPage();
      page.setDefaultTimeout(7000);
      const words=strings().I18N[lang]||{}, tr=s=>words[s]||s, f=fixture(lang), errors=[];
      page.on("pageerror",e=>errors.push(e.message));
      await page.route("**/*",f.route);
      await page.goto("http://magpie.test/?view=providers&edit=claude-team-"+org);
      const native=page.locator(".editor:not(.claude-team)");
      await native.locator(".team-accts .acc").nth(12).waitFor();
      assert.equal(await native.getByRole("button",{name:tr("Make first"),exact:true}).count(),0);
      assert.equal(await native.getByRole("button",{name:tr("Use"),exact:true}).count(),0);
      assert(!(await native.innerText()).includes(tr("Temporarily unable to read usage")));
      assert((await native.innerText()).includes("67%"),"real cached weekly usage appears");
      assert.equal(f.posts.length,0,"opening native subscription changes no seats");
      if(process.env.SCREENSHOT_DIR && lang==="zh") {
        await fs.mkdir(process.env.SCREENSHOT_DIR,{recursive:true});
        await page.evaluate(async()=>{ await Promise.all(document.getAnimations().map(a=>a.finished.catch(()=>{}))); });
        await page.screenshot({path:path.join(process.env.SCREENSHOT_DIR,engine+"-team-provider.png")});
      }
      await native.locator(".team-accts > button").click();
      const dialog=page.locator(".claude-team");
      await dialog.locator(".team-member").nth(12).waitFor();
      const geometry=await dialog.evaluate(e=>({
        body:getComputedStyle(e.querySelector(".ebody")).display,
        row:e.querySelector(".team-member").getBoundingClientRect().width,
        input:e.querySelector('.team-fields input[type="password"]').getBoundingClientRect().width,
        user:e.querySelector(".team-user").getBoundingClientRect().width,
      }));
      assert.equal(geometry.body,"flex","sections must not fall into native label grid");
      assert(geometry.row>500 && geometry.input>300 && geometry.user>150,JSON.stringify(geometry));
      assert.equal(await dialog.locator('input[type="password"]').inputValue(),"");
      await dialog.getByLabel(tr("Rotate at usage (%)"),{exact:true}).fill("95");
      await dialog.getByLabel(tr("Administrator sessionKey"),{exact:true}).fill("fixture-secret-value");
      await dialog.getByRole("button",{name:tr("Save"),exact:true}).click();
      await dialog.waitFor({state:"detached"});
      assert.deepEqual(f.posts[0].body,{name:"Glac Team",routing:"",enabled:true,organizationId:org,sessionKey:"fixture-secret-value",threshold:95,members:f.views.get(org).members.map(m=>m.accountId)});
      await page.locator('#providers .row[data-id="claude-team-'+org+'"]').click();
      await page.locator(".team-accts > button").click();
      await dialog.locator(".team-member").nth(12).waitFor();
      assert(!(await dialog.innerText()).includes("fixture-secret-value"));
      await dialog.getByRole("button",{name:tr("Rotate seat now"),exact:true}).click();
      await dialog.getByText(tr("Recent seat changes"),{exact:true}).waitFor();
      assert.deepEqual(f.posts[1],{path:"/api/claude-team/rotate",body:{organizationId:org,from:id(2)}});
      const failed=f.views.get(org).members[1];
      failed.quotaError="Quota collection failed; retry while this account has Premium access";
      await dialog.getByRole("button",{name:tr("Refresh"),exact:true}).click();
      await dialog.getByText(tr(failed.quotaError),{exact:true}).waitFor();
      const failedRow=dialog.locator('.team-member[data-account="'+id(3)+'"]');
      assert((await failedRow.innerText()).includes("67%"),"failure retains cached usage");
      await failedRow.getByRole("button",{name:tr("Retry usage collection"),exact:true}).click();
      await failedRow.getByText(tr("Collecting usage…"),{exact:true}).waitFor();
      assert.deepEqual(f.posts[2],{path:"/api/claude-team/quota",body:{organizationId:org,accountId:id(3)}});
      assert.equal(f.views.get(org).history.length,1,"retry never rotates seats");
      failed.quotaPending=false;
      if(process.env.SCREENSHOT_DIR && lang==="zh") {
        await fs.mkdir(process.env.SCREENSHOT_DIR,{recursive:true});
        await page.evaluate(async()=>{ await Promise.all(document.getAnimations().map(a=>a.finished.catch(()=>{}))); document.querySelector('.claude-team .ebody').scrollTop=0; });
        await page.screenshot({path:path.join(process.env.SCREENSHOT_DIR,engine+"-team-wide.png")});
      }
      await page.setViewportSize({width:380,height:780});
      assert(await dialog.evaluate(e=>e.scrollWidth<=e.clientWidth+1),"narrow modal does not overflow");
      assert(await dialog.locator(".team-user").first().evaluate(e=>e.getBoundingClientRect().width>130),"email is not a one-character column");
      await page.emulateMedia({colorScheme:"dark"});
      await page.addStyleTag({content:".claude-team {font-size:20px}"});
      assert(await dialog.evaluate(e=>e.scrollWidth<=e.clientWidth+1),"larger type does not overflow");
      if(process.env.SCREENSHOT_DIR && lang==="zh") {
        await dialog.locator('.team-member').first().scrollIntoViewIfNeeded();
        await page.screenshot({path:path.join(process.env.SCREENSHOT_DIR,engine+"-team-narrow.png")});
      }
      await dialog.getByRole("button",{name:tr("Cancel"),exact:true}).click();
      await dialog.waitFor({state:"detached"});
      await page.setViewportSize({width:1000,height:850});
      await page.locator("#addProvider").click();
      await page.locator('#addSheet .tile[data-pick="Claude Team"]').click();
      await dialog.getByRole("button",{name:second,exact:true}).waitFor();
      await dialog.getByRole("button",{name:second,exact:true}).click();
      await dialog.getByLabel(tr("Name"),{exact:true}).fill("Another organization");
      await dialog.getByRole("button",{name:tr("Save"),exact:true}).click();
      await dialog.waitFor({state:"detached"});
      await page.locator('#providers .row[data-id="claude-team-'+second+'"]').waitFor();
      assert.equal(f.providers.length,2,"adding an organization does not replace the first");
      assert.equal(f.posts[3].body.organizationId,second);
      assert.deepEqual(f.posts[3].body.members,[id(71),id(72)]);
      assert.equal(f.views.get(org).config.threshold,95,"other organization remains unchanged");
      assert.deepEqual(errors,[]);
    });
    test(engine+" "+lang+": saved usage failure leaves retry usable", {timeout:30000}, async t=>{
      const browser=await(engine==="webkit"?webkit.launch():chromium.launch({channel:"chromium"}));
      t.after(()=>browser.close());
      const page=await(await browser.newContext({viewport:{width:1000,height:850},reducedMotion:"reduce"})).newPage();
      const words=strings().I18N[lang]||{}, tr=s=>words[s]||s, f=fixture(lang);
      f.failCaptureOnSave=true;
      await page.route("**/*",f.route);
      await page.goto("http://magpie.test/?view=providers&edit=claude-team-"+org);
      await page.locator(".team-accts > button").click();
      const dialog=page.locator(".claude-team");
      await dialog.locator(".team-member").nth(12).waitFor();
      await dialog.getByRole("button",{name:tr("Save"),exact:true}).click();
      await page.waitForFunction(()=>document.querySelector(".claude-team .bar .primary")?.disabled===false);
      const row=dialog.locator('.team-member[data-account="'+id(3)+'"]');
      await row.getByText(tr("Quota collection failed; retry while this account has Premium access"),{exact:true}).waitFor();
      const retry=row.getByRole("button",{name:tr("Retry usage collection"),exact:true});
      assert(await retry.isEnabled(),"save completed but the failed member's retry stayed disabled");
      await retry.click();
      await row.getByText(tr("Collecting usage…"),{exact:true}).waitFor();
      assert.deepEqual(f.posts.map(p=>p.path),["/api/claude-team/config","/api/claude-team/quota"]);
    });
  }
}
