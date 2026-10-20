import assert from 'node:assert/strict';
import { test } from 'node:test';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync, renameSync, readdirSync, statSync, copyFileSync, symlinkSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const entry = join(here, 'git-canonical-gate');
const producer = 'pr-verify-merge:repository-gate:v1';
const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('GIT_') && key !== 'GITHUB_TOKEN' && key !== 'GH_TOKEN' && key !== 'NODE_TEST_CONTEXT'));
const native = process.env.CANONICAL_GATE_TEST_NATIVE === '1';

function run(file, args, cwd, extra = {}) {
  return spawnSync(file, args, { cwd, env: { ...env, ...extra }, encoding: 'utf8', timeout: 30_000 });
}
function git(cwd, ...args) {
  const result = run('git', args, cwd);
  assert.equal(result.status, 0, `fixture git ${args[0]} failed`);
  return result.stdout.trim();
}
function fixture(t, repository = 'example/canonical-gate-fixture') {
  const root = mkdtempSync(join(tmpdir(), 'canonical-gate-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const repo = join(root, 'repo');
  mkdirSync(repo);
  git(repo, 'init', '-q', '-b', 'main');
  git(repo, 'config', 'user.name', 'Gate fixture');
  git(repo, 'config', 'user.email', 'fixture@example.invalid');
  git(repo, 'config', 'commit.gpgsign', 'false');
  git(repo, 'config', 'core.hooksPath', join(repo, '.git/hooks'));
  git(repo, 'remote', 'add', 'origin', `https://github.com/${repository}.git`);
  writeFileSync(join(repo, 'source.txt'), 'base\n');
  git(repo, 'add', 'source.txt');
  git(repo, 'commit', '-qm', 'test: base');
  const base = git(repo, 'rev-parse', 'HEAD');
  git(repo, 'update-ref', 'refs/remotes/origin/main', base);
  writeFileSync(join(repo, 'source.txt'), 'head\n');
  git(repo, 'add', 'source.txt');
  git(repo, 'commit', '-qm', 'test: head');
  const head = git(repo, 'rev-parse', 'HEAD');
  const receiptPath = join(repo, '.git/rice-gates', `${head}-${base}.json`);
  return { root, repo, base, head, receiptPath, repository };
}
function mock(t, expression, repository) {
  const f = fixture(t, repository);
  const root = join(f.root, 'mock-producer');
  mkdirSync(join(root, 'scripts/lib'), { recursive: true });
  writeFileSync(join(root, 'scripts/lib/merge-hygiene.js'), `module.exports.runRepositoryGate = (root, target) => {\nrequire('node:assert/strict').deepEqual(target, ${JSON.stringify({ head_sha: f.head, base_sha: f.base, head_repo: f.repository, base_repo: f.repository, head_ref: 'main', base_ref: 'main' })});\nreturn (${expression});\n};\n`);
  git(f.repo, 'config', '--local', 'rice.canonicalGateRoot', root);
  return f;
}
const valid = `{ producer: '${producer}', head_sha: target.head_sha, base_sha: target.base_sha, repository: target.base_repo, exit_status: 0 }`;
function receipt(f) { return JSON.parse(readFileSync(f.receiptPath, 'utf8')); }

if (!native) {
  test('help, usage, dry-run and missing dependency codes', (t) => {
    const f = fixture(t);
    const help = run(entry, ['--help'], f.repo);
    assert.equal(help.status, 0);
    assert.match(help.stdout, /^Usage: git-canonical-gate /);
    assert.match(help.stdout, /--dry-run/);
    assert.equal(run(entry, ['--unknown'], f.repo).status, 2);
    const dry = run(entry, ['--dry-run'], f.repo);
    assert.equal(dry.status, 0);
    assert.match(dry.stdout, /plan:/);
    assert.equal(existsSync(join(f.repo, '.git/rice-gates')), false);
    const empty = join(f.root, 'empty');
    mkdirSync(empty);
    assert.equal(run(process.env.BASH || '/bin/bash', [entry], f.repo, { PATH: empty }).status, 3);
  });
  test('unconfigured gate delegates to actual just ci and preserves its exit code', (t) => {
    const f = fixture(t);
    writeFileSync(join(f.repo, 'justfile'), 'ci:\n    @printf "fallback-native\\n"\n    @exit 7\n');
    const result = run(entry, [], f.repo);
    assert.equal(result.status, 7);
    assert.match(result.stdout, /fallback-native/);
    assert.equal(existsSync(join(f.repo, '.git/rice-gates')), false);
  });
  for (const repository of ['example/canonical-gate-fixture', 'other-owner/varied-repository']) {
    test(`mock-only six-field target and receipt retention: ${repository}`, (t) => {
      const f = mock(t, valid, repository);
      const result = run(entry, [], f.repo, { GIT_DIR: '/nonexistent', GIT_WORK_TREE: '/nonexistent', GIT_INDEX_FILE: '/nonexistent' });
      assert.equal(result.status, 0, result.stderr);
      assert.match(result.stdout, /reused=false/);
      assert.equal(receipt(f).producer, producer);
      assert.equal(statSync(f.receiptPath).mode & 0o777, 0o600);
    });
  }
  for (const [label, expression] of [
    ['wrong producer', `({ ...${valid}, producer: 'untrusted' })`],
    ['wrong head', `({ ...${valid}, head_sha: '0'.repeat(40) })`],
    ['wrong base', `({ ...${valid}, base_sha: '0'.repeat(40) })`],
    ['wrong repository', `({ ...${valid}, repository: 'other/repository' })`],
    ['nonzero exit', `({ ...${valid}, exit_status: 9 })`],
    ['missing exit', `({ ...${valid}, exit_status: undefined })`],
    ['string exit', `({ ...${valid}, exit_status: '0' })`],
    ['error on zero exit', `({ ...${valid}, error: 'failed' })`],
    ['null receipt', 'null'],
  ]) {
    test(`mock-only fail closed and retain actual returned result: ${label}`, (t) => {
      const f = mock(t, expression);
      writeFileSync(join(f.repo, 'justfile'), 'ci:\n    @touch .git/forbidden-fallback\n');
      const result = run(entry, [], f.repo);
      assert.equal(result.status, 1);
      assert.equal(existsSync(join(f.repo, '.git/forbidden-fallback')), false);
      assert.equal(existsSync(f.receiptPath), true);
    });
  }
  test('configured gate and real install doctor work without optional fallback just', (t) => {
    const f = mock(t, valid);
    const home = join(f.root, 'home');
    const registry = join(f.root, 'registry');
    const prefix = join(home, '.local/bin');
    const thin = join(f.root, 'thin-path');
    const tool = join(registry, 'tools/git-canonical-gate');
    for (const dir of [prefix, thin, tool, join(registry, 'bin'), join(registry, 'registry')]) mkdirSync(dir, { recursive: true });
    copyFileSync(join(here, '../../registry/wanted.json'), join(registry, 'registry/wanted.json'));
    const gitPath = run('bash', ['-c', 'command -v git'], f.repo).stdout.trim();
    symlinkSync(gitPath, join(thin, 'git'));
    symlinkSync(process.execPath, join(thin, 'node'));
    const isolated = { PATH: `${thin}:${prefix}`, HOME: home, XDG_CONFIG_HOME: join(home, '.config'), XDG_STATE_HOME: join(home, '.state'), AGENT_DRY_ROOT: registry, AGENT_DRY_CONFIG: join(f.root, 'config.json') };
    assert.equal(run('/bin/bash', [entry], f.repo, isolated).status, 0);
    git(f.repo, 'config', '--local', '--unset', 'rice.canonicalGateRoot');
    assert.equal(run('/bin/bash', [entry], f.repo, isolated).status, 3);
    for (const name of ['tool.json', 'git-canonical-gate']) copyFileSync(join(here, name), join(tool, name));
    const cli = join(registry, 'bin/agent-dry');
    copyFileSync(join(here, '../../bin/agent-dry'), cli);
    writeFileSync(isolated.AGENT_DRY_CONFIG, JSON.stringify({ root: registry }));
    const installed = run(cli, ['install'], f.repo, isolated);
    assert.equal(installed.status, 0, installed.stdout + installed.stderr);
    const doctor = run(cli, ['doctor', '--json'], f.repo, isolated);
    assert.equal(doctor.status, 0, doctor.stdout + doctor.stderr);
    assert.equal(JSON.parse(doctor.stdout).healthy, true);
  });
  test('configured unavailable producer never falls back, including empty config', (t) => {
    const f = fixture(t);
    writeFileSync(join(f.repo, 'justfile'), 'ci:\n    @touch .git/forbidden-fallback\n');
    for (const value of ['', join(f.root, 'absent')]) {
      git(f.repo, 'config', '--local', 'rice.canonicalGateRoot', value);
      assert.equal(run(entry, [], f.repo).status, 1);
      assert.equal(existsSync(join(f.repo, '.git/forbidden-fallback')), false);
    }
  });
  test('producer exceptions and credential-bearing Git values do not enter diagnostics', (t) => {
    const f = mock(t, `(() => { throw new Error('sensitive-fixture-marker'); })()`);
    let result = run(entry, [], f.repo);
    assert.equal(result.status, 1);
    assert.doesNotMatch(result.stdout + result.stderr, /sensitive-fixture-marker/);
    git(f.repo, 'remote', 'set-url', 'origin', 'https://fixture-user:sensitive-fixture-marker@github.com/example/repository.git');
    result = run(entry, [], f.repo);
    assert.equal(result.status, 1);
    assert.doesNotMatch(result.stdout + result.stderr, /sensitive-fixture-marker/);
    git(f.repo, 'remote', 'remove', 'origin');
    result = run(entry, [], f.repo);
    assert.equal(result.status, 1);
    assert.doesNotMatch(result.stdout + result.stderr, /fatal:|Command failed:/);
  });
} else {
  test('native Git retry uses the unchanged real producer cache, never an imported receipt', (t) => {
    const producerRoot = process.env.CANONICAL_GATE_NATIVE_ROOT;
    assert.ok(producerRoot && existsSync(join(producerRoot, 'scripts/lib/merge-hygiene.js')), 'real producer required; no mock fallback');
    const f = fixture(t, `example/native-${process.pid}-${Date.now()}`);
    const bare = join(f.root, 'remote.git');
    git(f.root, 'init', '--bare', '-q', bare);
    git(f.repo, 'config', 'remote.origin.pushurl', bare);
    git(f.repo, 'push', '-q', 'origin', `${f.base}:refs/heads/main`);
    writeFileSync(join(f.repo, 'justfile'), 'ci:\n    @node --test fixture.test.mjs\n');
    writeFileSync(join(f.repo, 'fixture.test.mjs'), `import assert from 'node:assert/strict';\nimport {test} from 'node:test';\nimport {readFileSync,appendFileSync,existsSync} from 'node:fs';\ntest('actual fixture gate',()=>{appendFileSync('.git/gate-runs','run\\n');assert.equal(readFileSync('source.txt','utf8'),'head\\n');assert.equal(existsSync('.git/fail-gate'),false);});\n`);
    git(f.repo, 'add', 'justfile', 'fixture.test.mjs');
    git(f.repo, 'commit', '-qm', 'test: actual native gate');
    f.head = git(f.repo, 'rev-parse', 'HEAD');
    f.receiptPath = join(f.repo, '.git/rice-gates', `${f.head}-${f.base}.json`);
    git(f.repo, 'config', '--local', 'rice.canonicalGateRoot', producerRoot);
    writeFileSync(join(f.repo, '.git/hooks/pre-push'), `#!/usr/bin/env bash\nexec ${JSON.stringify(entry)}\n`, { mode: 0o755 });
    const reject = join(bare, 'hooks/pre-receive');
    writeFileSync(reject, '#!/bin/sh\nprintf "fixture transport rejection after successful pre-push\\n" >&2\nexit 1\n', { mode: 0o755 });
    const cacheHome = join(f.root, 'cache');
    const push = () => run('git', ['push', 'origin', 'main'], f.repo, { XDG_CACHE_HOME: cacheHome });
    const first = push();
    assert.notEqual(first.status, 0);
    assert.match(first.stdout + first.stderr, /canonical gate passed; reused=false/);
    assert.match(first.stderr, /fixture transport rejection/);
    const initial = receipt(f);
    assert.equal(initial.producer, producer);
    assert.equal(initial.exit_status, 0);
    assert.ok(initial.cache_key);
    assert.notEqual(initial.reused, true);
    assert.equal(git(bare, 'rev-parse', 'refs/heads/main'), f.base);
    const proofDir = process.env.CANONICAL_GATE_PROOF_DIR;
    if (proofDir) {
      mkdirSync(proofDir, { recursive: true });
      writeFileSync(join(proofDir, 'first-receipt.json'), readFileSync(f.receiptPath), { mode: 0o600 });
    }
    renameSync(reject, `${reject}.disabled`);
    const retry = push();
    assert.equal(retry.status, 0, retry.stderr);
    assert.match(retry.stdout + retry.stderr, /canonical gate passed; reused=true/);
    const cached = receipt(f);
    assert.equal(cached.reused, true);
    assert.equal(cached.cache_key, initial.cache_key);
    assert.equal(cached.head_sha, f.head);
    assert.equal(cached.base_sha, f.base);
    assert.equal(readFileSync(join(f.repo, '.git/gate-runs'), 'utf8'), 'run\n');
    assert.equal(git(bare, 'rev-parse', 'refs/heads/main'), f.head);
    assert.ok(readdirSync(cacheHome).length);
    if (proofDir) writeFileSync(join(proofDir, 'cached-retry-receipt.json'), readFileSync(f.receiptPath), { mode: 0o600 });
    console.log(`native producer PASS: first push rejected after genuine gate; retry reused=${cached.reused}; native gate executions=1; receipt producer=${cached.producer}`);
  });
}
