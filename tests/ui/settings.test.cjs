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
  const ctx = vm.createContext({$:get, authEnabled:false, lastAuthState:null, liveGate:false,
    renderKeyHint(){}, renderCredStatus(){}, setAuthTab(){}});
  vm.runInContext(functionSource('applyAuthUi'), ctx);
  vm.runInContext(functionSource('showAuthCard'), ctx);
  return {ctx,get};
}
test('settings and trading have separate forms; market inputs retain ownership', () => {
  const rail = html.match(/<aside\b[\s\S]*?<\/aside>/)[0];
  assert.match(rail, /id="credentials-form"/);
  assert.doesNotMatch(rail, /name="(?:symbol|strategy|execute|initial_cash)"/);
  for(const name of ['symbol','futures','leverage','allow_short']) {
    assert.match(html, new RegExp(`form="run-form" name="${name}"`));
  }
  assert.match(script, /Array\.from\(\$\("#run-form"\)\.elements\)/);
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
