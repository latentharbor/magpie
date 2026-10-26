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
      account:{agent:"claude",agentName:"Claude Code",agentIcon:"claude-color",user:v.members[0]?.user || "",plan:"Team",
        logins:v.members.map(m=>({agent:"claude",user:m.user,on:m.on,active:false,plan:"Team"}))}};
  }
  providers.push(provider(views.get(org)));
  const f = {posts,views,providers, starts:[]};
  f.route = async route => {
    const req=route.request(), url=new URL(req.url()), json=data=>route.fulfill({json:data});
    if(url.pathname==="/boot.js") return route.fulfill({contentType:"text/javascript",body:"window.bootPrefs="+JSON.stringify({lang,theme:"light",web:true})+";"});
    if(url.pathname==="/wails/runtime.js") return route.fulfill({contentType:"text/javascript",body:"export const Window={};"});
    if(url.pathname==="/api/state") return json({agents:[],profiles:[],settings:{lang,theme:"light"}});
    if(url.pathname==="/api/providers") return json({providers,presets:[],excluded:[],gateway:{running:true,window:true}});
    if(url.pathname==="/api/groups") return json({groups:[]});
    if(url.pathname==="/api/signin") {
      const body=req.postDataJSON(); f.starts.push(body);
      f.signing={id:"team-login-"+f.starts.length,agent:"claude",state:"waiting",url:"https://example.test/login",pasteCode:true};
      return json(f.signing);
    }
    if(url.pathname.startsWith("/api/signin/")) return json(f.signing);
    if(url.pathname.startsWith("/api/claude-team/")) {
      if(url.pathname.endsWith("/discover")) return f.discoveryError
        ? route.fulfill({status:502,json:{error:f.discoveryError}})
        : json(f.organizations || [{id:second,name:"Second Team"}]);
      if(req.method()==="POST") {
        const body=req.postDataJSON(); posts.push({path:url.pathname,body});
        const v=views.get(body.organizationId);
        if(url.pathname.endsWith("create")) {
          v.config.enabled=false;
          providers.push(provider(v));
        } else if(url.pathname.endsWith("config")) {
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
    test(engine+" "+lang+": elapsed Team windows show estimates and an unstarted 5h", {timeout:30000}, async t=>{
      const browser=await(engine==="webkit"?webkit.launch():chromium.launch({channel:"chromium"}));
      t.after(()=>browser.close());
      const page=await browser.newPage({viewport:{width:1000,height:850},reducedMotion:"reduce"});
      const words=strings().I18N[lang]||{}, tr=s=>words[s]||s, f=fixture(lang);
      const m=f.views.get(org).members[1], reset=Math.floor(Date.now()/1000)-6*24*3600;
      m.limits=[{kind:"five_hour",used:1,resetsAt:reset},{kind:"seven_day",used:1,resetsAt:reset}];
      // The API retains historical limits separately from its current projection.
      m.windows=[{name:"5 hours",used:0,estimated:true},
        {name:"7 days",used:0,estimated:true,resetsAt:new Date((reset+7*24*3600)*1000).toISOString()}];
      await page.route("**/*",f.route);
      await page.goto("http://magpie.test/?view=providers&edit=claude-team-"+org);
      const row=page.locator(".team-accts .acc").nth(1);
      await row.getByText(tr("Window not started"),{exact:true}).waitFor();
      assert.equal(await row.locator(".aq b").allTextContents().then(xs=>xs.filter(x=>x.includes("0%")).length),2);
      assert((await row.innerText()).includes(tr("Reset-based estimate; not refreshed from Claude")));
      assert(!(await row.innerText()).includes("100%"),"historical 100% presented as current usage");
      await page.locator(".team-accts > button").first().click();
      const member=page.locator('.team-member[data-account="'+m.accountId+'"]');
      await member.getByText(tr("Window not started"),{exact:true}).waitFor();
      assert.deepEqual(await member.locator("progress").evaluateAll(xs=>xs.map(x=>x.value)),[0,0]);
      assert((await member.innerText()).includes(tr("Reset-based estimate; not refreshed from Claude")));
      const next=await page.evaluate(at=>new Date(at).toLocaleString(intlLang()),m.windows[1].resetsAt);
      assert((await member.innerText()).includes(next),"fixed weekly deadline is visible");
      assert.equal(f.posts.length,0,"displaying a reset changed seats");
      assert.equal(m.limits[0].used,1,"display discarded historical data");
    });
    test(engine+" "+lang+": add inside an organization, retry and return to its account list", {timeout:30000}, async t=>{
      const browser=await(engine==="webkit"?webkit.launch():chromium.launch({channel:"chromium"}));
      t.after(()=>browser.close());
      const page=await(await browser.newContext({viewport:{width:1000,height:850},reducedMotion:"reduce"})).newPage();
      page.setDefaultTimeout(7000);
      const words=strings().I18N[lang]||{}, tr=s=>words[s]||s, f=fixture(lang), errors=[];
      page.on("pageerror",e=>errors.push(e.message));
      await page.route("**/*",f.route);
      await page.goto("http://magpie.test/?view=providers&edit=claude-team-"+org);
      const native=page.locator(".editor:not(.claude-team)");
      await native.locator(".team-add-account").click();
      await page.getByRole("button",{name:tr("Sign in anyway"),exact:true}).click();
      await page.locator(".signing .signlink").waitFor();
      assert.deepEqual(f.starts,[{agent:"claude",organizationId:org}]);
      f.signing.state="failed"; f.signing.error="fixture failure";
      await page.getByRole("button",{name:tr("Try again"),exact:true}).click();
      await page.locator(".signing .signlink").waitFor();
      assert.deepEqual(f.starts[1],f.starts[0],"retry lost the organization");
      const v=f.views.get(org), m={...v.members[0],accountId:id(60),user:"new@example.test",quotaPending:true};
      v.members.push(m); v.config.members.push(m.accountId);
      // A provider refresh, not a manual Team save, delivers the joined login.
      f.providers[0].account.logins.push({agent:"claude",user:m.user,on:true,plan:"Team"});
      f.signing={...f.signing,state:"done",user:m.user};
      await native.locator(".team-accts .acc").nth(13).waitFor();
      assert((await native.innerText()).includes(m.user));
      assert.equal(f.posts.length,0,"login required a separate Team save");
      m.quotaPending=false;
      m.windows=[{name:"5 hours",used:12},{name:"7 days",used:34}];
      await native.locator(".team-accts .acc").filter({hasText:m.user}).locator(".aq b").filter({hasText:"34%"}).waitFor();
      await native.locator(".team-accts > button").first().click();
      const dialog=page.locator(".claude-team");
      await dialog.locator(".team-add-account").waitFor();
      const extra={...m,accountId:id(61),user:"outside-login@example.test"};
      v.members.push(extra); v.config.members.push(extra.accountId);
      await dialog.getByRole("button",{name:tr("Refresh"),exact:true}).click();
      const row=dialog.locator('.team-member[data-account="'+extra.accountId+'"]');
      assert(await row.locator('input[type="checkbox"]').isChecked(),"refresh lost server-side automatic membership");
      await dialog.locator(".team-add-account").click();
      const confirm = page.getByRole("button",{name:tr("Sign in anyway"),exact:true});
      try { await confirm.click(); }
      catch (error) {
        console.error("Sign-in layout", await confirm.evaluate(b => ({
          button: b.getBoundingClientRect().toJSON(),
          modal: document.querySelector("#modal .dialog").getBoundingClientRect().toJSON(),
          bodies: [...document.querySelectorAll("#modal .ebody")].map(e => ({top:e.scrollTop,height:e.clientHeight,scroll:e.scrollHeight})),
          animations: document.getAnimations().map(a => ({state:a.playState,time:a.currentTime,end:a.effect?.getComputedTiming().endTime})),
        })));
        if (process.env.SCREENSHOT_DIR) {
          await fs.mkdir(process.env.SCREENSHOT_DIR,{recursive:true});
          await page.screenshot({path:path.join(process.env.SCREENSHOT_DIR,engine+"-"+lang+"-signin-failure.png")});
        }
        throw error;
      }
      await page.locator(".signing .signlink").waitFor();
      assert.equal(f.starts[2].organizationId,org,"settings entry lost the organization");
      assert.deepEqual(errors,[]);
    });
    test(engine+" "+lang+": organization discovery choices and explicit fallback", {timeout:30000}, async t=>{
      const browser=await(engine==="webkit"?webkit.launch():chromium.launch({channel:"chromium"}));
      t.after(()=>browser.close());
      const page=await browser.newPage({viewport:{width:1000,height:850},reducedMotion:"reduce"});
      page.setDefaultTimeout(7000);
      const words=strings().I18N[lang]||{}, tr=s=>words[s]||s, f=fixture(lang);
      f.organizations=[{id:org,name:"Existing Team"},{id:second,name:"Second Team"}];
      await page.route("**/*",f.route);
      await page.goto("http://magpie.test/?view=providers");
      const open=async()=>{
        if (!await page.locator("#addSheet").isVisible()) await page.locator("#addProvider").click();
        await page.locator('#addSheet .tile[data-pick="Claude Team"]').click();
        const sheet=page.locator(".team-create");
        await sheet.getByLabel(tr("Administrator sessionKey"),{exact:true}).fill("fixture-admin");
        return sheet;
      };
      let sheet=await open();
      await sheet.getByRole("button",{name:tr("Continue"),exact:true}).click();
      await sheet.locator('input[type="radio"]').nth(1).check();
      assert.equal(f.posts.length,0,"discovery saved credentials or created a provider");
      await sheet.getByRole("button",{name:tr("Cancel"),exact:true}).click();
      await sheet.waitFor({state:"detached"});
      f.discoveryError="Organization discovery unavailable";
      sheet=await open();
      await sheet.getByRole("button",{name:tr("Continue"),exact:true}).click();
      await sheet.getByText(f.discoveryError,{exact:true}).waitFor();
      assert.equal(await sheet.getByLabel(tr("Administrator sessionKey"),{exact:true}).inputValue(),"fixture-admin","fallback lost the session draft");
      await sheet.getByLabel(tr("Organization UUID"),{exact:true}).fill(second);
      await sheet.getByRole("button",{name:tr("Continue"),exact:true}).click();
      await sheet.waitFor({state:"detached"});
      await page.locator('#providers .row[data-id="claude-team-'+second+'"]').waitFor();
      assert.deepEqual(f.posts,[{path:"/api/claude-team/create",body:{organizationId:second,sessionKey:"fixture-admin"}}]);
    });
    test(engine+" "+lang+": a session creates an editable provider before accounts", {timeout:30000}, async t=>{
      const browser=await(engine==="webkit"?webkit.launch():chromium.launch({channel:"chromium"}));
      t.after(()=>browser.close());
      const page=await browser.newPage({viewport:{width:1000,height:850},reducedMotion:"reduce"});
      const words=strings().I18N[lang]||{}, tr=s=>words[s]||s, f=fixture(lang), errors=[];
      page.on("pageerror",e=>errors.push(e.message));
      f.views.get(second).members=[];
      f.views.get(second).config.members=[];
      await page.route("**/*",f.route);
      await page.goto("http://magpie.test/?view=providers");
      await page.locator("#addProvider").click();
      await page.locator('#addSheet .tile[data-pick="Claude Team"]').click();
      const sheet=page.locator(".team-create");
      await sheet.getByLabel(tr("Administrator sessionKey"),{exact:true}).fill("fixture-admin");
      await sheet.getByRole("button",{name:tr("Continue"),exact:true}).click();
      await sheet.waitFor({state:"detached"});
      const native=page.locator(".editor:not(.claude-team)");
      await native.locator(".team-add-account").waitFor();
      assert.equal(await native.locator(".team-accts .with-aq").count(),0,"an empty organization invented a signed-in account");
      await native.locator(".team-accts > button").first().click();
      await page.locator(".claude-team .team-fields").waitFor();
      assert.equal(f.posts.length,1,"opening the provider or its settings changed seats");
      assert.deepEqual(errors,[]);
    });
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
      await native.locator(".team-accts > button").first().click();
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
      await page.locator(".team-accts > button").first().click();
      await dialog.locator(".team-member").nth(12).waitFor();
      assert(!(await dialog.innerText()).includes("fixture-secret-value"));
      await dialog.getByRole("button",{name:tr("Rotate seat now"),exact:true}).click();
      await dialog.getByText(tr("Recent seat changes"),{exact:true}).waitFor().catch(async error => {
        const layout = await dialog.evaluate(e => ({
          connected:e.isConnected, history:e.querySelector(".team-history")?.outerHTML,
          bodyScroll:e.querySelector(".ebody")?.scrollTop,
          buttons:[...e.querySelectorAll("button")].filter(b=>b.textContent.includes("Rotate")).map(b=>({
            text:b.textContent,disabled:b.disabled,rect:b.getBoundingClientRect().toJSON(),
          })),
        }));
        throw new Error(JSON.stringify({posts:f.posts,layout}),{cause:error});
      });
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
      const create=page.locator(".team-create");
      await create.getByLabel(tr("Administrator sessionKey"),{exact:true}).fill("fixture-second-admin");
      assert.equal(await create.locator(".team-pool, .team-routing").count(),0,"creation still asks for rotation and accounts");
      await create.getByRole("button",{name:tr("Continue"),exact:true}).click();
      await dialog.waitFor({state:"detached"});
      await page.locator('#providers .row[data-id="claude-team-'+second+'"]').waitFor();
      assert.equal(f.providers.length,2,"adding an organization does not replace the first");
      assert.equal(f.posts[3].body.organizationId,second);
      assert.deepEqual(f.posts[3],{path:"/api/claude-team/create",body:{organizationId:second,sessionKey:"fixture-second-admin"}});
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
      await page.locator(".team-accts > button").first().click();
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
