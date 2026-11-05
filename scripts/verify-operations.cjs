#!/usr/bin/env node
// Exercise the installed CLI through real systemd user services, isolated from
// the owner's installation. Run after building; no privileged operations.
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const crypto = require('node:crypto');
const {spawnSync} = require('node:child_process');
const {DatabaseSync} = require('node:sqlite');
const net = require('node:net');
const assert = require('node:assert/strict');

function command(executable, args, options = {}) {
  const result = spawnSync(executable, args, {encoding:'utf8', timeout:120_000,
    stdio:['ignore','pipe','pipe'], ...options});
  if (result.error || result.status !== 0) throw new Error(`${path.basename(executable)} ${args[0]} failed (${result.status}): ${result.stderr || result.error?.message}`);
  return result.stdout;
}
async function main() {
  const archive = path.resolve(process.argv[2] || `rigscope-${require('../package.json').version}.tgz`);
  const fixture = fs.mkdtempSync(path.join(os.tmpdir(), 'rigscope-operations-'));
  fs.chmodSync(fixture, 0o700);
  const state = path.join(fixture, 'state');
  const prefix = path.join(fixture, 'prefix');
  const unitBase = `rigscope-${crypto.createHash('sha256').update(state).digest('hex').slice(0,12)}`;
  const componentUnits = ['hub', 'collector'].map(component => `${unitBase}-${component}.service`);
  const units = [...componentUnits, `${unitBase}.service`];
  const unitDirectory = path.join(process.env.XDG_CONFIG_HOME || path.join(os.homedir(), '.config'), 'systemd/user');
  const componentUnitFiles = componentUnits.map(unit => path.join(unitDirectory, unit));
  const env = {...process.env, XDG_BIN_HOME:path.join(fixture, 'bin')};
  for (const key of Object.keys(env)) if (/^(HUB_|COLLECTOR_|RIGSCOPE_)/.test(key)) delete env[key];
  let password;
  try {
    command('npm', ['install','--prefix',prefix,'--omit=dev','--ignore-scripts','--no-audit','--no-fund',archive]);
    const root = path.join(prefix,'node_modules','rigscope');
    const cli = path.join(root,'bin','rigscope.cjs');
    const invoke = (args, expected = 0) => {
      const approvals = ['install','upgrade','rollback','recover-password','uninstall'].includes(args[0]) ? ['--yes'] : [];
      const result = spawnSync(process.execPath, [cli,...args,...approvals,'--state-dir',state],
        {encoding:'utf8', timeout:120_000, env});
      assert.equal(result.status, expected, `CLI ${args[0]}: ${result.stderr}`);
      return result.stdout;
    };
    const listener = net.createServer();
    await new Promise(resolve => listener.listen(0,'127.0.0.1',resolve));
    const port = listener.address().port;
    await new Promise(resolve => listener.close(resolve));
    const version = JSON.parse(fs.readFileSync(path.join(root,'package.json'))).version;
    const installed = invoke(['install','--port',String(port)]);
    password = /Password: ([^\n]+)/.exec(installed)?.[1];
    assert.ok(password, 'fixture install prints its generated password once');
    const base = `http://127.0.0.1:${port}`;
    const waitHealthy = async () => {
      const deadline = Date.now()+25_000;
      while (Date.now()<deadline) {
        try {
          const response = await fetch(base+'/api/health',{signal:AbortSignal.timeout(1000)});
          const body = await response.json();
          if (response.ok && body.status==='ok' && body.version===version) return;
        } catch {}
        await new Promise(resolve => setTimeout(resolve,100));
      }
      throw new Error('isolated service never became healthy');
    };
    const login = value => fetch(base+'/api/auth/login',{method:'POST',headers:{Origin:base,'Content-Type':'application/json'},
      body:JSON.stringify({username:'owner',password:value}),signal:AbortSignal.timeout(5000)});
    invoke(['start']);
    await waitHealthy();
    const firstLogin = await login(password);
    assert.equal(firstLogin.status,200);
    const cookie = firstLogin.headers.get('set-cookie').split(';')[0];
    await firstLogin.body.cancel();
    const configBefore = fs.readFileSync(path.join(state,'config.json'),'utf8');
    const identity = () => {const s=JSON.parse(fs.readFileSync(path.join(state,'collector/state.json')));return JSON.stringify([s.collectorId,s.destinations]);};
    const identityBefore = identity();
    invoke(['upgrade','--from',archive,'--timeout','15']);
    await waitHealthy();
    const database = path.join(state,'hub/telemetry.sqlite');
    let db = new DatabaseSync(database);
    db.exec("CREATE TABLE cli_preservation_fixture (value TEXT); INSERT INTO cli_preservation_fixture VALUES ('keep');");
    db.close();
    invoke(['rollback','--timeout','15']);
    db = new DatabaseSync(database,{readOnly:true});
    assert.equal(db.prepare('SELECT value FROM cli_preservation_fixture').get().value,'keep');
    db.close();
    const broken = path.join(fixture,'broken');fs.mkdirSync(broken);
    command('tar',['-xzf',archive,'-C',broken]);
    const manifest = path.join(broken,'package/package.json');
    const pkg = JSON.parse(fs.readFileSync(manifest));pkg.version='0.1.999';
    fs.writeFileSync(manifest,JSON.stringify(pkg));
    fs.writeFileSync(path.join(broken,'package/dist/hub.cjs'),"throw new Error('intentional isolated startup failure');\n");
    const badArchive = path.join(fixture,'broken.tgz');
    command('tar',['-czf',badArchive,'-C',broken,'package']);
    const failed = invoke(['upgrade','--from',badArchive,'--timeout','2'],1);
    assert.ok(!failed.includes(password), 'no password in upgrade output');
    await waitHealthy();
    const current = JSON.parse(fs.readFileSync(path.join(state,'runtime/node_modules/rigscope/package.json')));
    assert.equal(current.version,version,'startup failure recovers the previous runtime');
    assert.equal(fs.readFileSync(path.join(state,'config.json'),'utf8'),configBefore);
    assert.equal(identity(),identityBefore,'identity, credentials and sharing survive upgrade/rollback');
    invoke(['stop']);
    const passwordFile = path.join(fixture,'recovery-password');
    fs.writeFileSync(passwordFile,'isolated-fixture-new-password',{mode:0o600});
    invoke(['recover-password','--password-file',passwordFile]);
    assert.equal(fs.readFileSync(path.join(state,'config.json'),'utf8'),configBefore,'recovery does not change bootstrap credentials');
    invoke(['start']);await waitHealthy();
    assert.equal((await login(password)).status,401,'old fixture password is invalid');
    const recoveredLogin = await login('isolated-fixture-new-password');
    assert.equal(recoveredLogin.status,200,'new password works from durable store');
    await recoveredLogin.body.cancel();
    assert.equal((await fetch(base+'/api/live',{headers:{cookie},signal:AbortSignal.timeout(5000)})).status,401,'old sessions stay invalid');
    const doctor = JSON.parse(invoke(['doctor','--json']));
    assert.equal(doctor.ok,true,'installed doctor accepts healthy local role');
    assert.ok(!JSON.stringify(doctor).includes(password));
    db = new DatabaseSync(database,{readOnly:true});
    assert.equal(db.prepare("SELECT count(*) n FROM admin_audit WHERE action='user.password-recovery'").get().n,1);
    assert.equal(db.prepare('PRAGMA quick_check').get().quick_check,'ok');db.close();
    invoke(['uninstall','--keep-data']);
    assert.ok(componentUnitFiles.every(file => !fs.existsSync(file)), 'uninstall removes both component units');
    assert.ok(fs.existsSync(database), 'uninstall retains history');
    assert.equal(fs.readFileSync(path.join(state,'config.json'),'utf8'), configBefore);
    invoke(['install']);
    assert.ok(componentUnitFiles.every(file => fs.existsSync(file)), 'retained installation registers both component units again');
    console.log('Installed operations: real user service install/start; verified upgrade; compatible rollback with data preservation; failed-start recovery; offline password recovery/audit/session invalidation; doctor — passed.');
  } finally {
    password = undefined;
    for (const unit of units) {
      spawnSync('systemctl',['--user','stop',unit],{stdio:'ignore'});
      spawnSync('systemctl',['--user','disable',unit],{stdio:'ignore'});
      fs.rmSync(path.join(unitDirectory, unit),{force:true});
      spawnSync('systemctl',['--user','reset-failed',unit],{stdio:'ignore'});
    }
    spawnSync('systemctl',['--user','daemon-reload'],{stdio:'ignore'});
    fs.rmSync(fixture,{recursive:true,force:true});
  }
}
main().catch(error=>{console.error(error.message);process.exitCode=1;});
