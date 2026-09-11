// 看板渲染冒烟测试（零依赖，只需 node）。
//
//   node scripts/render-smoke.mjs
//
// 做两件事：
//   1. 用 DOM 桩把 master/web/app.js 真正执行一遍，确认没有运行时错误；
//   2. 喂一份真实的节点样本，检查卡片/详情表格渲染出的关键内容是否正确，
//      并顺带验证 HTML 转义没有漏（防 XSS）。
//
// 这不能替代浏览器里的视觉验收，但能挡住"改完前端一片白屏"这类事故。

import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const appPath = path.join(root, 'master', 'web', 'app.js');
const code = fs.readFileSync(appPath, 'utf8');

let failures = 0;
const check = (name, cond, extra = '') => {
  if (cond) {
    console.log(`  [通过] ${name}`);
  } else {
    failures++;
    console.log(`  [失败] ${name}${extra ? ' → ' + extra : ''}`);
  }
};

// ---- DOM 桩：只要够 app.js 顶层代码跑通即可 ----
const el = () => ({
  innerHTML: '', textContent: '', className: '', style: {}, dataset: {},
  classList: { add() {}, remove() {}, contains: () => false },
  addEventListener() {}, querySelector: () => el(), querySelectorAll: () => [],
  setAttribute() {}, getAttribute: () => null, appendChild() {}, focus() {},
  getBoundingClientRect: () => ({ left: 0, top: 0, width: 720, height: 132 }),
});

const sandbox = {
  console,
  setInterval: () => 0,
  clearInterval: () => {},
  setTimeout: () => 0,
  fetch: () => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) }),
  document: {
    querySelector: () => el(),
    querySelectorAll: () => [],
    addEventListener: () => {},
    documentElement: { dataset: {} },
    title: '',
  },
  window: { matchMedia: () => ({ matches: false, addEventListener: () => {} }) },
  localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  encodeURIComponent,
  Math, Date, Number, String, Array, Object, JSON, Promise, Error, isNaN,
};
sandbox.globalThis = sandbox;

console.log('== 看板渲染冒烟测试 ==');

let ctx;
try {
  ctx = vm.createContext(sandbox);
  vm.runInContext(code, ctx, { filename: 'app.js' });
  check('app.js 在 DOM 桩环境下可正常加载执行（无顶层运行时错误）', true);
} catch (e) {
  check('app.js 在 DOM 桩环境下可正常加载执行（无顶层运行时错误）', false, e.message);
  process.exit(1);
}

// ---- 构造一份贴近真实的样本（含恶意字符串，用于验证转义） ----
const sample = {
  node_id: 'web-01"><img src=x onerror=alert(1)>',
  labels: { env: 'prod', zone: 'cn-hz-a' },
  ts: Math.floor(Date.now() / 1000),
  seq: 1234,
  agent_version: '1.0.0',
  interval_sec: 10,
  online: true,
  age_sec: 4,
  stale_after_sec: 30,
  host: {
    hostname: 'web-01', os: 'linux', arch: 'amd64', kernel: '6.8.0-40-generic',
    container: 'docker', uptime_sec: 987654,
  },
  cpu: { usage_pct: 42.5, cores: 16, threads: 32, sockets: 2, load1: 1.23, load5: 1.1, load15: 0.9, freq_mhz: 3200, temp_c: 58.3 },
  mem: { total_bytes: 68719476736, used_bytes: 40802189312, used_pct: 59.4, swap_total_bytes: 8589934592, swap_used_bytes: 1073741824 },
  disk: {
    max_used_pct: 78.2, total_bytes: 1099511627776, used_bytes: 859547867136,
    mounts: [
      { mount: '/', fs: 'ext4', total_bytes: 536870912000, used_bytes: 419647569920, avail_bytes: 117223342080, used_pct: 78.2, inode_pct: 12.1 },
      { mount: '/data', fs: 'xfs', total_bytes: 536870912000, used_bytes: 268435456000, avail_bytes: 268435456000, used_pct: 50.0 },
    ],
  },
  net: { rx_bytes_sec: 1234567, tx_bytes_sec: 7654321, interfaces: [{ name: 'eth0', rx_bytes_sec: 1234567, tx_bytes_sec: 7654321 }] },
  gpu: [
    { index: 0, name: 'NVIDIA GeForce RTX 4090', util_pct: 87.5, mem_total_bytes: 25769803776, mem_used_bytes: 12884901888, mem_used_pct: 50.0, temp_c: 71, power_w: 312.5, fan_pct: 60 },
    { index: 1, name: 'NVIDIA GeForce RTX 4090', util_pct: 12.0, mem_total_bytes: 25769803776, mem_used_bytes: 1073741824, mem_used_pct: 4.2, temp_c: 45, power_w: 80 },
  ],
  temps: [
    { name: 'coretemp · Package id 0', kind: 'cpu', source: 'hwmon', temp_c: 58.3, max_c: 84.0, crit_c: 100.0 },
    { name: 'nvme Composite', kind: 'disk', source: 'hwmon', temp_c: 41.0, max_c: 65.0, crit_c: 85.0 },
    { name: 'nvme Composite', kind: 'disk', source: 'hwmon', temp_c: 88.5, max_c: 65.0, crit_c: 85.0 },
    { name: 'acpitz', kind: 'board', source: 'thermal', temp_c: 30.0 },
    { name: 'NVIDIA GeForce RTX 4090', kind: 'gpu', source: 'nvidia-smi', temp_c: 71.0 },
  ],
  max_temp_c: 88.5,
  temp: {
    max_c: 88.5, max_name: 'nvme Composite', max_kind: 'disk', sensor_num: 5,
    by_kind: { cpu: 58.3, disk: 88.5, board: 30.0, gpu: 71.0 },
  },
  collect_ms: 23,
  errors: ['gpu: 调用 nvidia-smi 失败: context deadline exceeded'],
};

const html = ctx.cardHTML(sample);

check('卡片渲染不抛异常', typeof html === 'string' && html.length > 200);
check('包含节点名', html.includes('web-01'));
check('包含在线状态', html.includes('在线'));
check('CPU 使用率已格式化', html.includes('42.5%'));
check('卡片同时显示物理核与逻辑线程', html.includes('16 核 32 线程'));
check('内存绝对值已格式化', html.includes('38.0 GiB') && html.includes('64.0 GiB'));
check('磁盘使用率取最紧张挂载点', html.includes('78.2%'));
check('GPU 型号已渲染', html.includes('RTX 4090'));
check('采集降级信息已展示', html.includes('采集降级'));
check('标签已渲染', html.includes('env=prod') && html.includes('zone=cn-hz-a'));
check('运行时长已格式化', html.includes('11 天'));

// XSS：node_id / 错误信息里的尖括号必须被转义
check('node_id 中的 HTML 被转义（防 XSS）', !html.includes('<img src=x'), html.slice(0, 160));
check('转义后的实体存在', html.includes('&lt;img src=x'));

const info = ctx.infoTable(sample);
check('详情信息表渲染正常', info.includes('web-01') && info.includes('16 核') && info.includes('58.3 °C'));
check('信息表核心与线程分列', info.includes('CPU 物理核心') && info.includes('16 核') &&
  info.includes('CPU 逻辑线程') && info.includes('32 线程'));
check('多路机器才显示路数', info.includes('2 路'));

// 关键回归：agent 识别不出物理核心时（cores=0），绝不能拿线程数冒充核心数。
const noCore = { ...sample, cpu: { ...sample.cpu, cores: 0 } };
const htmlNoCore = ctx.cardHTML(noCore);
const infoNoCore = ctx.infoTable(noCore);
check('识别不出物理核时只报线程数', htmlNoCore.includes('32 线程') && !htmlNoCore.includes('32 核'),
  htmlNoCore.slice(0, 160));
check('识别不出物理核时不显示核心行', !infoNoCore.includes('CPU 物理核心'));
check('单路机器不显示路数', !ctx.infoTable({ ...sample, cpu: { ...sample.cpu, sockets: 1 } }).includes(' 路'));

// cpuTopoText 的三条规则
check('核=线程时只说核', ctx.cpuTopoText({ cores: 8, threads: 8 }) === '8 核');
check('无超线程数据时只说核', ctx.cpuTopoText({ cores: 8, threads: 0 }) === '8 核');
check('无任何拓扑时返回空串', ctx.cpuTopoText({}) === '' && ctx.cpuTopoText(null) === '');

const disks = ctx.diskTable(sample);
check('磁盘表包含两个挂载点', disks.includes('/') && disks.includes('/data'));
check('磁盘表包含 inode 列', disks.includes('12.1%'));

const gpus = ctx.gpuTable(sample);
check('GPU 表包含两块卡', (gpus.match(/RTX 4090/g) || []).length === 2);
check('GPU 功耗已渲染', gpus.includes('313 W') || gpus.includes('312 W'));

// ---- 温度 ----
check('卡片渲染温度条', html.includes('磁盘 温度') && html.includes('89°C'), html.includes('磁盘 温度'));
check('高温传感器触发卡片告警条', html.includes('温度告警'));
check('卡片温度条最多 3 行，其余折叠', html.includes('另有 1 类传感器'));

const temps = ctx.tempTable(sample);
check('温度表列出全部 5 个传感器',
  temps.includes('coretemp') && temps.includes('acpitz') &&
  (temps.match(/<tr>/g) || []).length === 6);
check('温度表展示来源与临界值', temps.includes('hwmon') && temps.includes('nvidia-smi') && temps.includes('100°C'));
check('温度表按类别排序（CPU 在前，主板在后）',
  temps.indexOf('coretemp') < temps.indexOf('nvme') && temps.indexOf('nvme') < temps.indexOf('acpitz'));
check('无温度传感器时给出可执行的提示',
  ctx.tempTable({ node_id: 'x', host: {} }).includes('管理员'));

// 兼容 agent v1.0.x：只有 cpu.temp_c / gpu[].temp_c，没有 temps 数组
const legacyTemps = ctx.tempTable({ cpu: { temp_c: 55 }, gpu: [{ index: 0, name: 'RTX 4090', temp_c: 60 }] });
check('老版本 agent 只有 CPU/GPU 温度时仍可渲染',
  legacyTemps.includes('55°C') && legacyTemps.includes('RTX 4090'));

check('温度类别按固定顺序输出',
  ctx.tempKindsOf(sample).join(',') === 'cpu,gpu,disk,board',
  ctx.tempKindsOf(sample).join(','));

const panelKeys = ctx.drawerPanels(sample).map((p) => p.key);
check('抽屉面板顺序：使用率 → GPU → 温度 → 网络',
  panelKeys.join(',') === 'cpu,mem,disk,gpu,temp,temp:cpu,temp:gpu,temp:disk,temp:board,__net',
  panelKeys.join(','));
check('无温度传感器的节点不生成温度图表面板',
  ctx.drawerPanels({ node_id: 'x', host: {} }).every((p) => p.key.indexOf('temp') !== 0));

const head = ctx.drawerHeaderHTML(sample);
check('抽屉头部摘要正常', head.includes('web-01') && head.includes('agent 1.0.0'));

// 边界：全空样本不应抛异常（新节点第一次上报只有主机信息）
try {
  ctx.cardHTML({ node_id: 'bare', online: false, age_sec: 5, stale_after_sec: 30, host: {}, errors: [] });
  check('极简样本（无 CPU/内存/磁盘/GPU/温度）不抛异常', true);
} catch (e) {
  check('极简样本（无 CPU/内存/磁盘/GPU/温度）不抛异常', false, e.message);
}

// ---- loadCharts 全流程 ----
// 上面都是纯函数，这一段会真的走 DOM 写入 + 多次 fetch + 图表渲染，
// 是前端最容易"改完一片白屏"的地方。state 是 const 词法绑定，
// 不能通过 ctx 直接改，所以用 runInContext 在同一上下文里赋值。
sandbox.fetch = () => Promise.resolve({
  ok: true, status: 200,
  json: () => Promise.resolve({ unit: '°C', points: [{ ts: 1700000000, v: 40 }, { ts: 1700000060, v: 47 }] }),
});
vm.runInContext(
  'state.selected = ' + JSON.stringify(sample.node_id) + ';' +
  'state.nodes = [' + JSON.stringify(sample) + '];',
  ctx,
);

try {
  await ctx.loadCharts();
  check('loadCharts 全流程可执行（含温度面板、网络双线、XSS 样本）', true);
} catch (e) {
  check('loadCharts 全流程可执行（含温度面板、网络双线、XSS 样本）', false, e.message);
}

try {
  vm.runInContext('state.selected = "bare"; state.nodes = [{ node_id: "bare", host: {} }];', ctx);
  await ctx.loadCharts();
  check('loadCharts 对无任何指标的节点也可执行', true);
} catch (e) {
  check('loadCharts 对无任何指标的节点也可执行', false, e.message);
}

console.log('');
if (failures === 0) {
  console.log('结论：全部通过。');
  process.exit(0);
}
console.log(`结论：${failures} 项失败。`);
process.exit(1);
