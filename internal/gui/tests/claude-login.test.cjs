// Isolated real-page tests; no live gateway, credentials or certificate trust.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { test } = require('node:test');
const { chromium, webkit } = require('playwright');
const assets = path.resolve(__dirname, '../assets');
for (const engine of ['chromium','webkit']) for (const lang of ['en','zh','zh-TW','ja','de']) {
 test(`${engine} ${lang}: Claude login mapping, trust gate, drafts and narrow layout`, async t => {
  const browser = await (engine === 'webkit' ? webkit.launch() : chromium.launch());
  t.after(()=>browser.close());
  const page = await browser.newPage({ viewport:{width:1000,height:760},reducedMotion:'reduce' });
  page.setDefaultTimeout(5000);
  const errors=[],posts=[];page.on('pageerror',e=>errors.push(e.message));
  let cur={config:{enabled:false,listen:'127.0.0.1:3426',models:{'claude-opus-5-5':'deleted-team/claude-opus-5-5'}},runtime:{running:false},certificate:{trusted:false,path:'/Users/fixture/.config/magpie/claude-login-ca/ca.pem',sha256:'a'.repeat(64)},supported:true};
  await page.route('**/*',async route=>{
   const req=route.request(),u=new URL(req.url()),json=data=>route.fulfill({json:data});
   if(u.pathname==='/boot.js')return route.fulfill({contentType:'text/javascript',body:`window.bootPrefs=${JSON.stringify({lang,theme:'light',web:true})};`});
   if(u.pathname==='/wails/runtime.js')return route.fulfill({contentType:'text/javascript',body:'export const Window={};'});
   if(u.pathname==='/api/state')return json({agents:[{id:'claude-desktop',name:'Claude Desktop',icon:'claude-color',path:'/fixture/Claude',wired:true,loginProxy:true,fields:[{key:'provider',label:'provider',value:'magpie',options:[{value:'magpie',label:'magpie'}]}]}],profiles:[],settings:{lang,theme:'light'}});
   if(u.pathname==='/api/settings')return json({lang,theme:'light',tray:'panel',currency:'usd',textSize:100,proxy:'',proxyNow:'',proxySource:'none',visionModels:[],imageGenModels:[],workbuddyCheckins:[],fx:{rate:7.2,at:new Date().toISOString()}});
   if(u.pathname==='/api/claude-login/prepare-usage'){cur.usageCertificate={trusted:true,path:'/fixture/web-ca/ca.pem'};return json(cur);}
   if(u.pathname==='/api/claude-login/prepare'){cur.certificate={...cur.certificate,exists:true,sha256:'a'.repeat(64)};return json(cur);}
   if(u.pathname==='/api/claude-login'){
    if(req.method()==='POST'){posts.push(req.postDataJSON());cur={...cur,config:posts.at(-1)};}
    return json(cur);
   }
   if(u.pathname==='/api/plugins')return json({plugins:[]});
   if(u.pathname==='/api/usage/quotas')return json([]);
   if(u.pathname==='/api/groups')return json({groups:[{id:'opus',ref:'group/opus',name:'Opus',ready:true}],models:[{id:'team-new/claude-opus-5-5',provider:'team-new',providerName:'New Team',name:'Opus 5.5'}]});
   if(u.pathname.startsWith('/api/'))return json({});
   const f=path.join(assets,u.pathname==='/'?'index.html':u.pathname);
   return route.fulfill({body:await fs.readFile(f),contentType:{'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'}[path.extname(f)]});
  });
  await page.goto('http://magpie.test/?view=settings&tab=network');
  await page.locator('#claudeLoginOpen').click();
  const modal=page.locator('.claude-login');await modal.waitFor();
  await modal.locator('.login-target-error:not([hidden])').waitFor();
  const geometry=await modal.evaluate(e=>({display:getComputedStyle(e.querySelector('.ebody')).display,
    certificate:e.querySelector('.login-certificate').getBoundingClientRect().width,
    mapping:e.querySelector('.login-mapping input').getBoundingClientRect().width}));
  assert.equal(geometry.display,'flex','mapping editor inherited the label/control grid');
  assert(geometry.certificate>450 && geometry.mapping>140,JSON.stringify(geometry));
  await modal.getByRole('button',{name:await page.evaluate(()=>t('Map Claude models from a provider')),exact:true}).click();
  await page.getByRole('menuitemradio',{name:'New Team',exact:true}).click();
  assert.equal(await modal.locator('.login-mapping input').nth(1).inputValue(),'team-new/claude-opus-5-5','replacing a deleted Team kept its old target');
  assert.equal(await modal.locator('.login-target-error:not([hidden])').count(),0);
  const check=modal.locator('input[type=checkbox]').first();
  const usage=modal.locator('input[type=checkbox]').nth(1); await usage.check();await assert.doesNotReject(()=>check.waitFor());
  await page.waitForFunction(()=>document.querySelector('.claude-login input[type=checkbox]').disabled);
  const org=modal.locator('input[placeholder="00000000-0000-0000-0000-000000000000"]'); await org.fill('11111111-2222-4333-8444-555555555555');
  const inputs=modal.locator('.login-mapping input');
  await inputs.nth(0).fill('claude-opus-5-5');await inputs.nth(1).fill('group/opus');
  // Preparing a certificate never silently trusts it or discards model edits.
  await modal.locator('.bar button').nth(0).click();
  await modal.locator('.login-certificate').getByText('SHA-256:',{exact:false}).waitFor();
  assert.equal(await inputs.nth(0).inputValue(),'claude-opus-5-5');assert.equal(await check.isDisabled(),true);
  await modal.locator('.bar button').nth(1).click();
  assert.equal(await inputs.nth(1).inputValue(),'group/opus'); assert.equal(await usage.isChecked(),true);
  await modal.locator('.bar button').nth(2).click();
  assert.equal(await org.inputValue(),'11111111-2222-4333-8444-555555555555');
  await modal.locator('.bar button').last().click();
  await page.waitForFunction(()=>document.querySelector('.claude-login .bar button:last-child').disabled===false);
  assert.deepEqual(posts,[{enabled:false,gatewayUsage:true,usageOrganization:'11111111-2222-4333-8444-555555555555',listen:'127.0.0.1:3426',models:{'claude-opus-5-5':'group/opus'}}]);
  await page.setViewportSize({width:420,height:760});
  await page.waitForTimeout(120);
  assert.equal(await modal.evaluate(e=>e.scrollWidth<=e.clientWidth+1),true);
  assert(await modal.locator('.login-certificate').evaluate(e=>e.getBoundingClientRect().width>300),'certificate collapsed into a label column');
  assert.equal(await modal.locator('select').count(),0);
  if(process.env.SCREENSHOT_DIR && lang==='zh') {
    await fs.mkdir(process.env.SCREENSHOT_DIR,{recursive:true});
    await page.screenshot({path:path.join(process.env.SCREENSHOT_DIR,engine+'-login-narrow.png')});
    await page.setViewportSize({width:1000,height:900});
    await page.emulateMedia({colorScheme:'dark'});
    await page.screenshot({path:path.join(process.env.SCREENSHOT_DIR,engine+'-login-wide.png')});
  }
  await page.goto('http://magpie.test/?view=agents');
  const row=page.locator('.row.agent[data-id="claude-desktop"]');
  const expected=await page.evaluate(()=>t('Connected · official Claude login with selected models through the local proxy'));
  await row.getByText(expected,{exact:true}).waitFor();
  assert.equal(await row.locator('.ag-fix').count(),0);
  assert.deepEqual(errors,[]);
 });
}
