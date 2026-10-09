// Run: node --test tests/ui/settings.test.cjs
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const assets = path.join(__dirname, '../../internal/webui/static');
const script = fs.readFileSync(path.join(assets, 'app.js'), 'utf8');
const html = fs.readFileSync(path.join(assets, 'index.html'), 'utf8');
function functionSource(name) {
  const start = script.indexOf(`function ${name}(`);
  const end = script.indexOf('\n}', start);
  assert.ok(start >= 0 && end > start);
  return script.slice(start, end + 2);
}
function harness() {
  const nodes = new Map();
  const get = key => {
    if (!nodes.has(key)) nodes.set(key, {hidden:false, value:'', textContent:'', open:false,
      reset() { for(const [key,node] of nodes) if(key.includes('data-cred')) node.value=''; }});
    return nodes.get(key);
  };
  const ctx = vm.createContext({$:get, authEnabled:false, localSettings:false, lastAuthState:null,
    liveGate:false, liveGateEnv:false,
    renderKeyHint(){}, renderCredStatus(){}, setAuthTab(){}, renderAiPage(){}, localLiveHint(){}});
  vm.runInContext(functionSource('applyAuthUi'), ctx);
  vm.runInContext(functionSource('showAuthCard'), ctx);
  return {ctx,get};
}
test('settings and trading have separate forms; market inputs retain ownership', () => {
  const rail = html.match(/<aside\b[\s\S]*?<\/aside>/)[0];
  assert.match(rail, /id="credentials-form"/);
  assert.doesNotMatch(rail, /name="(?:symbol|strategy|execute|initial_cash)"/);
  for(const name of ['symbol','leverage','allow_short']) {
    assert.match(html, new RegExp(`form="run-form" name="${name}"`));
  }
  // Perpetual-only terminal: the spot toggle is gone and the form always
  // submits futures=true.
  assert.doesNotMatch(html, /name="futures"/);
  assert.match(script, /futures: true/);
  assert.match(script, /Array\.from\(\$\("#run-form"\)\.elements\)/);
});
test('strategy route is distinct and navigation retains form values', () => {
  const {ctx,get}=harness();
  const panels=[{hidden:false},{hidden:false}];
  ctx.window={location:{hash:'#/strategies'}};
  ctx.document={title:''};
  ctx.$$=()=>panels;
  get('.board').classList={toggle(){}};
  for(const id of ['#nav-market','#nav-strategies','#nav-ai']) get(id).setAttribute=()=>{};
  get('[name="initial_cash"]').value='12345';
  vm.runInContext(functionSource('renderPage'),ctx);
  ctx.renderPage();
  assert.equal(get('#trade-config').hidden,false);
  assert.equal(panels[0].hidden,true);
  ctx.window.location.hash='#/market'; ctx.renderPage();
  assert.equal(get('#trade-config').hidden,true);
  assert.equal(panels[0].hidden,false);
  assert.equal(get('[name="initial_cash"]').value,'12345');
});
test('disabled authentication does not display a login form', () => {
  const {ctx,get}=harness();
  ctx.applyAuthUi({auth:{enabled:false}});
  assert.equal(get('#auth-card').hidden,true);
  assert.equal(get('#creds-adv').hidden,true);
});
test('login populates account settings without expanding trade parameters', () => {
  const {ctx,get}=harness();
  ctx.applyAuthUi({auth:{enabled:true,username:'fixture',llm_base_url:'https://example.com/v1'}});
  assert.equal(get('#creds-adv').hidden,false);
  assert.equal(get('#advanced').open,false);
  assert.equal(get('[data-cred="llm_base_url"]').value,'https://example.com/v1');
});
test('expired login clears connection draft and reopens login entry', () => {
  const {ctx,get}=harness();
  get('[data-cred="llm_base_url"]').value='https://example.com/v1';
  ctx.showAuthCard();
  assert.equal(get('[data-cred="llm_base_url"]').value,'');
  assert.equal(get('#creds-adv').hidden,true);
  assert.equal(get('#settings-login').hidden,false);
});

// The desktop edition has no administrator and no login: its settings panel
// must be editable straight away. This is the regression guard for the bug
// where the credential form was permanently hidden and the page told the user
// to "ask an administrator" who does not exist.
test('desktop local mode opens the credential form without a login', () => {
  const {ctx,get}=harness();
  ctx.applyAuthUi({desktop:true, local_settings:true, live_gate:false, live_gate_env:false,
    auth:{enabled:true, local:true, username:'本机', path:'C:/Users/x/credentials.json'}});
  assert.equal(get('#creds-adv').hidden,false);
  assert.equal(get('#auth-card').hidden,true);
  assert.equal(get('#user-chip').hidden,true);
  assert.equal(get('#local-live-switch').hidden,false);
  assert.equal(get('#local-live').checked,false);
  assert.match(get('#settings-account').textContent,/本机/);
});

// On a networked server the live switch is not offered at all: the
// environment variable stays the only gate there.
test('server mode hides the desktop live switch', () => {
  const {ctx,get}=harness();
  ctx.applyAuthUi({desktop:false, local_settings:false, live_gate:true, live_gate_env:true,
    auth:{enabled:true, username:'trader'}});
  assert.equal(get('#local-live-switch').hidden,true);
  assert.equal(get('#creds-adv').hidden,false);
});

// The AI page must not claim a key it does not have. Before this guard the
// desktop page said "密钥来自本机设置" even with an empty Token field, which
// reads as "the AI is configured" when it is not.
test('AI page does not claim a model key that is missing', () => {
  const src = functionSource('renderAiPage');
  const nodes = new Map();
  const get = (key) => {
    if (!nodes.has(key)) nodes.set(key, {textContent:'', className:'', value:'ma_cross', disabled:false, hidden:false, checked:false});
    return nodes.get(key);
  };
  const ctx = vm.createContext({
    $:get,
    session:{running:false},
    strategyByName:()=>({title:'双均线交叉'}),
    localSettings:false,
  });
  vm.runInContext(src, ctx);
  ctx.renderAiPage({local_settings:true, auth:{enabled:true, local:true, username:'本机', llm_key:false},
    llm_model:'gpt-4o-mini'});
  assert.doesNotMatch(ctx.$("#ai-status").textContent, /密钥来自本机设置/);
  assert.match(ctx.$("#ai-status").textContent, /设置 → Binance 与 AI 服务/);

  // With a stored token the wording flips to the local file.
  ctx.renderAiPage({local_settings:true, auth:{enabled:true, local:true, username:'本机', llm_key:true},
    llm_model:'gpt-4o-mini'});
  assert.match(ctx.$("#ai-status").textContent, /密钥来自本机设置/);
});

// The model name must be pickable, not only typeable: the field carries a
// datalist fed by /api/models, and the button that fills it exists in the
// panel next to the URL it queries.
test('model field offers a fetched list instead of only free text', () => {
  const rail = html.match(/<aside\b[\s\S]*?<\/aside>/)[0];
  assert.match(rail, /id="load-models"/);
  assert.match(rail, /id="model-options"/);
  assert.match(rail, /data-cred="llm_model"[^>]*list="model-options"/);
  assert.match(script, /api\("\/api\/models"/);
  assert.match(script, /document\.getElementById\("model-options"\)/);
});

// The automatic fetch on load must not spam the provider: a config refresh
// (session poll re-render, login, etc.) may call applyAuthUi many times, and
// only the first may query.
test('model list is fetched at most once per page load', () => {
  const {ctx,get}=harness();
  let calls = 0;
  ctx.modelsLoaded = false;
  ctx.loadModels = () => { calls += 1; };
  const cfg = {local_settings:true, auth:{enabled:true, local:true, username:'本机', llm_key:true}};
  ctx.applyAuthUi(cfg);
  ctx.applyAuthUi(cfg);
  ctx.applyAuthUi(cfg);
  assert.equal(calls, 1);
  // Without a stored key there is nothing to query with.
  ctx.modelsLoaded = false;
  calls = 0;
  ctx.applyAuthUi({local_settings:true, auth:{enabled:true, local:true, username:'本机', llm_key:false}});
  assert.equal(calls, 0);
});
