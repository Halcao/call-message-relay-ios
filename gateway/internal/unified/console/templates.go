package console

// consoleTemplates is the complete server-rendered UI. It is deliberately
// dependency-free: no external JS/CSS, no token or hash material is ever
// rendered.
const consoleTemplates = `
{{define "layout"}}<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · CallRelay 控制台</title>
<style>
:root{color-scheme:dark;--bg:#101318;--panel:#181c23;--panel2:#202630;--line:#303844;--text:#edf1f7;--muted:#a4afbf;--accent:#82adff;--button:#346edb;--ok:#91dfbd;--warn:#edca89;--err:#ffa6ad;--radius:14px;--sidebar:216px}
*{box-sizing:border-box}
[hidden]{display:none!important}
html{scroll-padding-top:24px}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;-webkit-font-smoothing:antialiased}
::selection{background:#31538b;color:#fff}
a{color:var(--accent);text-decoration:none;text-underline-offset:4px}
a:hover{text-decoration:underline}
button,input,select,textarea,summary,a{touch-action:manipulation}
:focus-visible{outline:2px solid var(--accent);outline-offset:4px}
.skip-link{position:fixed;top:-80px;left:16px;z-index:20;padding:10px 18px;background:var(--panel2);border-radius:8px}
.skip-link:focus{top:12px}
header.top{position:fixed;inset:0 auto 0 0;width:var(--sidebar);display:flex;flex-direction:column;align-items:stretch;padding:32px 18px 20px;background:#14181e;border-right:1px solid var(--line);z-index:5}
.brand{display:flex;align-items:center;gap:11px;font-size:19px;font-weight:650;letter-spacing:-.02em;padding:0 10px 30px;color:var(--text)}
.brand-mark{width:29px;height:29px;color:var(--accent);flex:none}
.brand span{color:var(--text)}
nav.links{display:flex;flex-direction:column;gap:5px}
nav.links a{display:flex;align-items:center;gap:12px;min-height:44px;padding:10px 14px;border-radius:8px;color:var(--muted);font-weight:500;transition:background .16s ease-out,color .16s ease-out}
nav.links a:hover{background:var(--panel2);color:var(--text);text-decoration:none}
nav.links a.active{background:#23334d;color:#b6d0ff}
nav.links svg{width:18px;height:18px;flex:none}
nav.links a:nth-child(5),nav.links a:nth-child(8){margin-top:22px}
.user{margin-top:auto;padding:20px 10px 0;border-top:1px solid var(--line);display:flex;flex-wrap:wrap;align-items:center;gap:10px;color:var(--muted);font-size:12px;overflow-wrap:anywhere}
.user .account-name{flex:1;min-width:0}
.user button{min-height:34px;padding:5px 9px;font-size:12px}
main{margin-left:var(--sidebar);padding:42px 48px 28px;max-width:1640px;min-height:calc(100vh - 64px)}
h1{font-size:28px;line-height:1.3;font-weight:650;letter-spacing:-.025em;margin:0 0 28px}
h2{font-size:17px;line-height:1.4;font-weight:600;margin:32px 0 16px}
p{margin:12px 0;max-width:75ch}
.card{background:var(--panel);border:1px solid var(--line);border-radius:var(--radius);padding:24px;margin-bottom:20px;min-width:0}
.card>h2:first-child{margin-top:0}
.card>.row:first-child{gap:16px;margin-bottom:14px}
.card strong{font-size:16px;font-weight:600}
.grid{display:grid;gap:20px;grid-template-columns:repeat(auto-fit,minmax(min(220px,100%),1fr))}
.metrics{display:grid;grid-template-columns:repeat(5,minmax(0,1fr));gap:0;border-block:1px solid var(--line);margin-bottom:36px;padding:22px 0}
.stat{padding:0 24px;border-right:1px solid var(--line);display:flex;flex-direction:column-reverse;gap:8px}
.stat:first-child{padding-left:0}.stat:last-child{border-right:0}
.stat .num{font-size:26px;line-height:1.2;font-weight:600;font-variant-numeric:tabular-nums}
.stat .lbl{color:var(--muted);font-size:12px}
.table-scroll{overflow-x:auto;max-width:100%;scrollbar-color:#566173 transparent;scrollbar-width:thin;border-radius:8px}
table{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}
th,td{text-align:left;padding:15px 13px;border-bottom:1px solid var(--line);vertical-align:middle;font-size:13px;white-space:nowrap}
th{color:var(--muted);font-weight:500;font-size:12px;padding-top:8px;padding-bottom:13px}
tr:last-child td{border-bottom:0}
tr:hover td{background:#1c222c}
td[colspan]{padding:32px 16px;text-align:center;color:var(--muted)}
td .mono{display:block;margin-top:4px;font-size:11px}
.pill{display:inline-flex;align-items:center;gap:6px;vertical-align:middle;white-space:nowrap;padding:3px 9px;border-radius:6px;font-size:12px;line-height:1.6;background:var(--panel2);color:var(--muted)}
.pill.ok{color:var(--ok);background:#1c352d}.pill.err{color:var(--err);background:#3b262e}.pill.warn{color:var(--warn);background:#3a3224}
.pill.ok:before,.pill.err:before,.pill.warn:before{content:"";width:5px;height:5px;border-radius:50%;background:currentColor}
form.inline{display:inline}
label{display:block;color:var(--muted);font-size:13px;margin:12px 0 7px}
label:has(input[type=checkbox]){display:flex;align-items:center;gap:8px;color:var(--text)}
input[type=text],input[type=password],input[type=number],select,textarea{width:100%;min-width:0;min-height:44px;background:#12171e;color:var(--text);border:1px solid #414b5a;border-radius:8px;padding:10px 12px;font:inherit;caret-color:var(--accent);transition:border-color .16s ease-out}
input::placeholder,textarea::placeholder{color:#9aa6b8;opacity:1}
input:hover,select:hover,textarea:hover{border-color:#687991}
input:focus,select:focus,textarea:focus{border-color:var(--accent)}
input[type=checkbox]{width:17px;height:17px;accent-color:var(--button);flex:none;margin:0}
textarea{min-height:78px;resize:vertical;line-height:1.7}
button{background:var(--button);color:#fff;border:1px solid transparent;border-radius:8px;min-height:42px;padding:9px 17px;font:inherit;font-size:13px;font-weight:550;cursor:pointer;transition:background .16s ease-out,border-color .16s ease-out}
button:hover{background:#427cec}button:active{background:#285bbc}
button.ghost{background:transparent;border-color:#445064;color:var(--text)}
button.ghost:hover{background:var(--panel2);border-color:#718099}
button.danger{background:transparent;border-color:#74515b;color:var(--err)}
button.danger:hover{background:#422931;border-color:#a96b79}
button:disabled{opacity:.5;cursor:not-allowed}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:center}
.muted{color:var(--muted);font-size:13px}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;overflow-wrap:anywhere;white-space:normal}
.flash,.errbox{padding:14px 18px;border-radius:8px;margin-bottom:20px;overflow-wrap:anywhere}
.flash{background:#1c352d;color:var(--ok);border:1px solid #3b6655}
.errbox{background:#3b262e;color:var(--err);border:1px solid #74515b}
.secret{background:#1d2a3d;border-color:#435d83}
.qr{background:#fff;padding:12px;border-radius:12px;display:inline-block;flex:none}
.qr img{display:block;max-width:100%;height:auto}
.reveal-fields{padding:20px 0 4px}.reveal-fields img{max-width:220px;height:auto;border:10px solid #fff;border-radius:10px}
.reveal-fields .row{margin:10px 0}
.reveal-status:not(:empty){margin-top:12px;font-size:13px}
.reveal-status.ok{color:var(--ok)}.reveal-status.err{color:var(--err)}
.checkline{display:flex;gap:16px 24px;flex-wrap:wrap;margin:18px 0}
.checkline label{display:flex;align-items:center;gap:8px;color:var(--text);font-size:13px;margin:0;min-height:28px}
details{border-top:1px solid var(--line);margin-top:22px;padding-top:16px}
summary{cursor:pointer;color:var(--muted);font-size:13px;min-height:32px;line-height:32px}
summary:hover,details[open]>summary{color:var(--text)}
details[open]>summary{margin-bottom:16px}
.line-number{font-size:23px;font-weight:550;letter-spacing:-.015em;font-variant-numeric:tabular-nums;margin:10px 0 18px}
.number-form{max-width:520px;margin-bottom:14px}
.key-block>.row:first-child .mono{display:block;margin-top:5px;color:var(--muted);font-size:11px}
.key-block>.table-scroll{margin-block:16px}
.key-block>.muted{font-size:12px}
footer{margin-left:var(--sidebar);color:var(--muted);font-size:11px;padding:8px 48px 24px}
body.page-login main{margin:0;max-width:none;min-height:100vh;display:grid;place-items:center;padding:28px}
.loginwrap{width:100%;max-width:408px}
.loginwrap .brand{padding:0;margin-bottom:36px;justify-content:center;font-size:24px}
.loginwrap .card{padding:32px;margin:0}
.loginwrap h1{font-size:23px;margin-bottom:8px}
.loginwrap p{margin:0 0 26px}
.loginwrap button{width:100%;margin-top:12px;min-height:46px}
.loginwrap input{margin-bottom:12px}
.loginwrap .login-foot{text-align:center;font-size:12px;color:var(--muted);margin:24px 0 0}
@media(min-width:1640px){main{margin-right:auto}}
@media(max-width:1100px){:root{--sidebar:184px}header.top{padding-inline:12px}main{padding:32px 28px}.brand{font-size:17px}.metrics .stat{padding-inline:14px}.stat:first-child{padding-left:0}footer{padding-inline:28px}}
@media(max-width:760px){:root{--sidebar:0px}header.top{position:relative;width:auto;padding:18px 20px 0;display:grid;grid-template-columns:1fr auto;gap:0 12px;border-right:0;border-bottom:1px solid var(--line)}.brand{padding:0;font-size:18px;gap:9px}.brand-mark{width:25px;height:25px}.user{grid-column:2;grid-row:1;padding:0;border:0;margin:0;justify-content:flex-end}.user .account-name{display:none}nav.links{grid-column:1/-1;display:flex;flex-direction:row;overflow-x:auto;flex-wrap:nowrap;gap:4px;padding:16px 0 12px;scrollbar-width:thin}nav.links a{flex:none;gap:7px;min-height:40px;padding:8px 11px;font-size:13px}nav.links a:nth-child(5),nav.links a:nth-child(8){margin:0}nav.links svg{width:16px;height:16px}main{padding:28px 20px;min-height:calc(100vh - 195px)}h1{font-size:25px;margin-bottom:22px}h2{margin-top:26px}.card{padding:18px;margin-bottom:16px;border-radius:12px}.metrics{grid-template-columns:repeat(3,minmax(0,1fr));row-gap:22px;margin-bottom:26px;padding-block:18px}.metrics .stat{padding-inline:12px}.stat:first-child,.stat:nth-child(4){padding-left:0}.stat:nth-child(3){border:0}.stat .num{font-size:23px}.stat .lbl{font-size:11px}.grid{grid-template-columns:1fr;gap:10px}.table-scroll{margin-inline:0}th,td{padding:13px 10px}.row{gap:10px}.row>div{min-width:0;max-width:100%}.checkline{gap:12px 18px}.line-number{font-size:21px}.secret .row>div{min-width:0!important}.qr{margin-bottom:8px}footer{padding:0 20px 24px}body.page-login main{padding:24px}.loginwrap .card{padding:26px}.loginwrap .brand{margin-bottom:28px}}
@media(prefers-reduced-motion:reduce){*{transition:none!important;scroll-behavior:auto!important}}
</style>
</head>
<body class="page-{{.Page}}">
<a class="skip-link" href="#main">跳到内容</a>
{{if ne .Page "login"}}
<header class="top">
  <div class="brand"><svg class="brand-mark" viewBox="0 0 32 32" fill="none" aria-hidden="true"><path d="M6 18V9a3 3 0 0 1 3-3h9M14 26h9a3 3 0 0 0 3-3v-9M11 21 21 11M11 11h10v10" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/></svg><span>CallRelay</span></div>
  <nav class="links" aria-label="主导航">
    <a href="/console/" class="{{if eq .Page "overview"}}active{{end}}" {{if eq .Page "overview"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z"/></svg>总览</a>
    <a href="/console/lines" class="{{if eq .Page "lines"}}active{{end}}" {{if eq .Page "lines"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 4h14v16H5zM9 8h6M9 12h6M9 16h3"/></svg>线路</a>
    <a href="/console/keys" class="{{if eq .Page "keys"}}active{{end}}" {{if eq .Page "keys"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 4a6 6 0 1 1-4 10L4 20H2v-4l6-6a6 6 0 0 1 6-6ZM16 8h.01"/></svg>密钥</a>
    <a href="/console/devices" class="{{if eq .Page "devices"}}active{{end}}" {{if eq .Page "devices"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7 2h10v20H7zM11 18h2"/></svg>设备</a>
    <a href="/console/calls" class="{{if eq .Page "calls"}}active{{end}}" {{if eq .Page "calls"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 3H4v4c0 7 6 13 13 13h4v-4l-5-2-2 2a12 12 0 0 1-6-6l2-2z"/></svg>通话</a>
    <a href="/console/conferences" class="{{if eq .Page "conferences"}}active{{end}}" {{if eq .Page "conferences"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8ZM2 21v-3a6 6 0 0 1 12 0v3M17 4a4 4 0 0 1 0 8M17 15a5 5 0 0 1 5 5v1"/></svg>会议</a>
    <a href="/console/voicemails" class="{{if eq .Page "voicemails"}}active{{end}}" {{if eq .Page "voicemails"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8h12a4 4 0 1 0-4-4v4M10 12v4"/></svg>留言</a>
    <a href="/console/settings" class="{{if eq .Page "settings"}}active{{end}}" {{if eq .Page "settings"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 7h16M4 17h16M8 4v6M16 14v6"/></svg>设置</a>
    <a href="/console/diagnostics" class="{{if eq .Page "diagnostics"}}active{{end}}" {{if eq .Page "diagnostics"}}aria-current="page"{{end}}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 12h4l3-7 4 14 3-7h4"/></svg>诊断</a>
  </nav>
  <div class="user"><span class="account-name">{{.User}}</span>
    <form class="inline" method="post" action="/console/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="ghost" type="submit">退出</button></form>
  </div>
</header>
{{end}}
<main id="main">
{{if .Flash}}<div class="flash">{{.Flash}}</div>{{end}}
{{if .Error}}<div class="errbox">{{.Error}}</div>{{end}}
{{if eq .Page "login"}}{{template "login" .}}{{end}}
{{if eq .Page "overview"}}{{template "overview" .}}{{end}}
{{if eq .Page "lines"}}{{template "lines" .}}{{end}}
{{if eq .Page "keys"}}{{template "keys" .}}{{end}}
{{if eq .Page "devices"}}{{template "devices" .}}{{end}}
{{if eq .Page "calls"}}{{template "calls" .}}{{end}}
{{if eq .Page "conferences"}}{{template "conferences" .}}{{end}}
{{if eq .Page "voicemails"}}{{template "voicemails" .}}{{end}}
{{if eq .Page "settings"}}{{template "settings" .}}{{end}}
{{if eq .Page "diagnostics"}}{{template "diagnostics" .}}{{end}}
</main>
<script>
(function(){
 document.querySelectorAll('label').forEach(function(label,index){
  if(label.htmlFor||label.querySelector('input,select,textarea'))return;
  var field=label.nextElementSibling;
  if(field&&field.matches('input,select,textarea')){if(!field.id)field.id='field-'+index;label.htmlFor=field.id;}
 });
})();
</script>
{{if ne .Page "login"}}<footer>CallRelay · {{.GatewayID}}</footer>{{end}}
</body></html>{{end}}

{{define "login"}}
<div class="loginwrap">
  <div class="brand"><svg class="brand-mark" viewBox="0 0 32 32" fill="none" aria-hidden="true"><path d="M6 18V9a3 3 0 0 1 3-3h9M14 26h9a3 3 0 0 0 3-3v-9M11 21 21 11M11 11h10v10" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/></svg><span>CallRelay</span></div>
  <div class="card">
    <h1>登录控制台</h1>
    <p class="muted">登录以管理号码与设备。</p>
    <form method="post" action="/console/login">
      <label for="username">用户名</label><input id="username" type="text" name="username" required autocomplete="username">
      <label for="password">密码</label><input id="password" type="password" name="password" required autocomplete="current-password">
      <div style="margin-top:14px"><button type="submit">登录</button></div>
    </form>
    <p class="muted" style="margin-top:16px">使用手机/电脑拨打与收发短信？<a href="/pwa/client.html">打开网页版客户端</a>（无需登录控制台）。</p>
  </div>
</div>{{end}}

{{define "overview"}}
<h1>总览</h1>
<div class="metrics">
  <div class="stat"><div class="num">{{.OnlineCount}}/{{len .Lines}}</div><div class="lbl">在线线路</div></div>
  <div class="stat"><div class="num">{{.ActiveCalls}}</div><div class="lbl">进行中通话</div></div>
  <div class="stat"><div class="num">{{len .Devices}}</div><div class="lbl">已注册设备</div></div>
  <div class="stat"><div class="num">{{len .Conferences}}</div><div class="lbl">进行中会议</div></div>
  <div class="stat"><div class="num">{{len .Keys}}</div><div class="lbl">配对密钥</div></div>
</div>
<h2>线路状态</h2>
<div class="card"><div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
<tr><th>线路</th><th>号码</th><th>状态</th><th>SIM</th><th>运营商</th><th>语音</th><th>短信</th><th>当前通话</th></tr>
{{range .Lines}}<tr>
<td>{{if .Name}}{{.Name}}{{else}}未命名线路{{end}}</td>
<td class="mono">{{.NumberLabel}}</td>
<td>{{if .Online}}<span class="pill ok">在线</span>{{else}}<span class="pill err">离线</span>{{end}}</td>
<td>{{.SIM}}</td><td>{{.Operator}}</td><td>{{.Voice}}</td>
<td>{{if .SMSLive}}可用{{else}}不可用{{end}}</td>
<td class="mono">{{if .ActiveCallID}}{{.ActiveCallID}}{{else}}-{{end}}</td>
</tr>{{end}}
{{if not .Lines}}<tr><td colspan="8" class="muted">还没有配置线路</td></tr>{{end}}
</table></div></div>
<h2>进行中通话</h2>
<div class="card"><div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
<tr><th>线路</th><th>号码</th><th>方向</th><th>状态</th><th>开始</th></tr>
{{range .Calls}}<tr><td>{{if .LineName}}{{.LineName}}{{else}}{{lineName $.LineNames .LineID}}{{end}}</td><td class="mono">{{.Peer}}</td><td>{{directionLabel .Direction}}</td><td>{{stateLabel .State}}</td><td>{{ts .StartedAt}}</td></tr>{{end}}
{{if not .Calls}}<tr><td colspan="5" class="muted">当前没有通话</td></tr>{{end}}
</table></div></div>
<h2>最近事件</h2>
<div class="card"><div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
<tr><th>序号</th><th>类型</th><th>线路</th><th>时间</th></tr>
{{range .Events}}<tr><td>{{.seq}}</td><td>{{eventLabel .type}}</td><td>{{lineName $.LineNames .lineId}}</td><td>{{ts .createdAt}}</td></tr>{{end}}
</table></div></div>
{{end}}

{{define "lines"}}
<h1>线路</h1>
{{range .Lines}}
<div class="card">
  <div class="row" style="justify-content:space-between">
    <div><strong>{{if .Name}}{{.Name}}{{else}}未命名线路{{end}}</strong> <span class="muted mono">{{.ID}}</span>
      {{if .Identity.SIMChanged}}<span class="pill warn">SIM 已更换，待确认</span>{{end}}</div>
    <div>{{if .Online}}<span class="pill ok">在线</span>{{else}}<span class="pill err">离线</span>{{end}}
      {{if .SMSLive}}<span class="pill ok">短信可用</span>{{else}}<span class="pill">短信不可用</span>{{end}}</div>
  </div>
  <div class="line-number">{{.NumberLabel}}</div>
  <details><summary>模块与 SIM 信息</summary>
  <p class="muted mono">
    号码 {{.NumberLabel}}{{if .NumberKind}}（{{.NumberKind}}）{{end}}{{if .Identity.OperatorAlpha}} · 运营商 {{.Identity.OperatorAlpha}}{{end}}<br>
    模块标识 {{.Identity.ModuleKey}} · USB {{.Identity.ModuleUSB}} · 固件 {{.Identity.ModuleFirmware}}<br>
    SIM {{.Identity.SIMMasked}}
  </p></details>
  <form class="number-form" method="post" action="/console/lines/{{.ID}}/number">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <div class="grid">
      <div><label>电话号码（留空恢复自动读取）</label><input type="text" name="manual_number" inputmode="tel" placeholder="手动号码"></div>
    </div>
    <div class="row" style="margin-top:8px">
      <button type="submit">保存号码</button>
    </div>
  </form>
  <form class="inline" method="post" action="/console/lines/{{.ID}}/refresh-identity" style="margin-top:8px">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <button class="ghost" type="submit">重新读取 SIM 号码</button>
  </form>
  <details><summary>线路设置</summary>
  <form method="post" action="/console/lines/{{.ID}}">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <div class="grid">
      <div><label>名称</label><input type="text" name="name" value="{{.Name}}"></div>
      <div><label>录音留存天数</label><input type="number" name="recording_retention_days" min="0" max="3650" value="{{.RecordingRetentionDays}}"></div>
      <div><label>来电拦截策略</label>
        <select name="reject_policy">
          <option value="none" {{if eq .RejectPolicy "none"}}selected{{end}}>不拦截</option>
          <option value="reject_all" {{if eq .RejectPolicy "reject_all"}}selected{{end}}>拒接全部来电</option>
          <option value="reject_blocked" {{if eq .RejectPolicy "reject_blocked"}}selected{{end}}>拒接前缀黑名单</option>
        </select>
      </div>
      <div><label>黑名单前缀（英文逗号分隔，脱敏展示）</label><input type="text" name="blocked_prefixes" value="{{.BlockedPrefixes}}" placeholder="例如 400,95"></div>
    </div>
    <div class="checkline">
      <label><input type="checkbox" name="enabled" {{if .Enabled}}checked{{end}}> 启用线路</label>
      <label><input type="checkbox" name="allow_inbound" {{if .AllowInbound}}checked{{end}}> 允许来电</label>
      <label><input type="checkbox" name="allow_outbound" {{if .AllowOutbound}}checked{{end}}> 允许外呼</label>
      <label><input type="checkbox" name="sms_live" {{if .SMSLive}}checked{{end}}> 允许发送短信</label>
    </div>
    <div style="margin-top:14px"><button type="submit">保存线路设置</button></div>
  </form></details>
</div>
{{end}}
{{if not .Lines}}<div class="card muted">还没有配置线路。</div>{{end}}
{{end}}

{{define "keys"}}
<h1>配对密钥</h1>
{{if .NewKey}}
<div class="card secret key-block">
  <h2 style="margin-top:0">新密钥已创建</h2>
  <p>扫码或复制配对内容，在 App 中添加网关。之后可在此查看和复制。</p>
  <div class="row" style="align-items:flex-start">
    {{if .NewKey.QRDataURI}}<div class="qr"><img src="{{.NewKey.QRDataURI}}" width="220" height="220" alt="配对二维码"></div>{{end}}
    <div style="flex:1;min-width:260px">
      <label>密钥 ID</label><div class="mono">{{.NewKey.ID}}</div>
      <label>配对密钥</label>
      <textarea class="mono reveal-secret" readonly rows="2">{{.NewKey.ID}}.{{.NewKey.Secret}}</textarea>
      <div class="row"><button type="button" class="ghost copy-secret">复制密钥</button></div>
      <label>配对内容</label>
      <textarea class="mono reveal-payload" readonly rows="5">{{.NewKey.Payload}}</textarea>
      <div class="row"><button type="button" class="ghost copy-payload">复制配对内容</button></div>
      <div class="reveal-status muted" role="status"></div>
    </div>
  </div>
</div>
{{end}}
<div class="card" id="create-key">
  <h2 style="margin-top:0">创建密钥</h2>
  <form method="post" action="/console/keys">
    <input type="hidden" name="csrf" value="{{.CSRF}}">
    <div class="grid">
      <div><label>名称</label><input type="text" name="name" placeholder="例如 我的 iPhone"></div>
      <div><label>覆盖范围</label><label style="color:var(--text)"><input type="checkbox" name="all_lines" checked> 自动覆盖全部线路（含以后新增）</label></div>
    </div>
    <div class="checkline" style="margin-top:8px">
      <label><input type="checkbox" name="manage_number"> 允许手机编辑线路号码</label>
    </div>
    <div style="margin-top:12px"><button type="submit">生成密钥</button></div>
  </form>
</div>
<input type="hidden" id="keys-csrf" value="{{.CSRF}}">
{{range .Keys}}{{$key := .}}
<div class="card key-block" data-reveal-url="/console/keys/{{.Key.ID}}/reveal">
  <div class="row" style="justify-content:space-between">
    <div><strong>{{.Key.Name}}</strong> <span class="muted mono">{{.Key.ID}}</span></div>
    <div>{{if .Key.RevokedAt}}<span class="pill err">已撤销</span>{{else}}<span class="pill ok">有效</span>{{end}}</div>
  </div>
  <p class="muted">创建于 {{ts .Key.CreatedAt.UnixMilli}} · {{if .Key.AllLines}}默认覆盖全部线路{{else}}仅限显式授权线路{{end}} · {{if .Key.ManageNumber}}可管理线路自有号码{{else}}不能修改线路号码{{end}}
    {{if not .Key.RevokedAt}} · {{if .SecretRecoverable}}<span class="pill ok">可查看</span>{{else}}<span class="pill warn">无法查看</span>{{end}}{{end}}</p>
  {{if not .Key.RevokedAt}}
  <div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
    <tr><th>线路</th><th>收短信</th><th>接来电</th><th>发短信</th><th>外呼</th><th></th></tr>
    {{range .Grants}}<tr>
      <td>{{if .LineName}}{{.LineName}}{{else}}{{lineName $.LineNames .LineID}}{{end}} <span class="muted mono">{{.LineID}}</span></td>
      <td>{{if .ReceiveSMS}}✓{{else}}—{{end}}</td>
      <td>{{if .ReceiveCalls}}✓{{else}}—{{end}}</td>
      <td>{{if .SendSMS}}✓{{else}}—{{end}}</td>
      <td>{{if .Dial}}✓{{else}}—{{end}}</td>
      <td><form class="inline" method="post" action="/console/keys/{{$key.Key.ID}}/lines/{{.LineID}}/delete"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="ghost" type="submit">移除</button></form></td>
    </tr>{{end}}
    {{if not .Grants}}{{if .Key.AllLines}}<tr><td colspan="6" class="muted">此密钥自动覆盖全部线路（含以后新增），无需逐条授权</td></tr>{{else}}<tr><td colspan="6" class="muted">还没有授权任何线路</td></tr>{{end}}{{end}}
  </table></div>
  {{if .SecretRecoverable}}
  <div class="row" style="margin-top:10px">
    <button type="button" class="ghost reveal-button" data-reveal-url="/console/keys/{{.Key.ID}}/reveal">查看密钥</button>
  </div>
  <div class="reveal-fields" hidden>
    <label>配对密钥</label>
    <textarea class="mono reveal-secret" readonly rows="2"></textarea>
    <div class="row"><button type="button" class="ghost copy-secret">复制密钥</button></div>
    <label>配对内容</label>
    <textarea class="mono reveal-payload" readonly rows="5"></textarea>
    <div class="row" style="align-items:flex-start">
      <button type="button" class="ghost copy-payload">复制配对内容</button>
      <img class="qr reveal-qr" width="180" height="180" alt="配对二维码" hidden>
    </div>
    <div class="row" style="margin-top:6px">
      <button type="button" class="ghost hide-button">隐藏</button>
    </div>
  </div>
  <div class="reveal-status muted" role="status"></div>
  {{else}}
  <div class="errbox" style="background:#2b2313;border-color:#6e5220;color:#ffd79a;margin-top:10px">
    旧密钥未保存原文，无法查看，已连接设备不受影响。
    如需重新配对新手机，请先<a href="#create-key">创建新密钥</a>。
  </div>
  {{end}}
  <details><summary>授权与设置</summary>
  <form method="post" action="/console/keys/{{.Key.ID}}/manage-number" style="margin-top:10px">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <label>
      <input type="checkbox" name="manage_number" {{if .Key.ManageNumber}}checked{{end}}
             onchange="this.form.submit()"> 允许手机编辑线路号码
    </label>
    <noscript><button type="submit">保存号码管理授权</button></noscript>
  </form>
  <form method="post" action="/console/keys/{{$key.Key.ID}}/lines">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <div class="grid">
      <div><label>线路</label>
        {{if $.Lines}}<select name="line_id">
          {{range $.Lines}}<option value="{{.ID}}">{{if .Name}}{{.Name}}{{else}}{{.ID}}{{end}}{{if .Identity.PhoneMasked}} · {{.Identity.PhoneMasked}}{{end}}</option>{{end}}
        </select>{{else}}<p class="muted">还没有线路，请先在“线路”页配置。</p>{{end}}
      </div>
    </div>
    <div class="checkline">
      <label><input type="checkbox" name="receive_sms" checked> 收短信</label>
      <label><input type="checkbox" name="receive_calls" checked> 接来电</label>
      <label><input type="checkbox" name="send_sms" checked> 发短信</label>
      <label><input type="checkbox" name="dial" checked> 外呼</label>
    </div>
    <div style="margin-top:10px"><button type="submit">保存线路授权</button></div>
  </form>
  <form method="post" action="/console/keys/{{.Key.ID}}/revoke" onsubmit="return confirm('撤销后所有关联设备立即失效，确定？')">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <div style="margin-top:10px"><button class="danger" type="submit">撤销密钥</button></div>
  </form></details>
  {{else}}
  <div class="row" style="margin-top:10px;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap">
    <span class="muted">此密钥已撤销。</span>
    <form class="inline" method="post" action="/console/keys/{{.Key.ID}}/delete" onsubmit="return confirm('永久删除该已撤销密钥及其线路授权？设备与历史记录会保留，且不可恢复密钥内容。')">
      <input type="hidden" name="csrf" value="{{$.CSRF}}">
      <button class="danger" type="submit">删除密钥</button>
    </form>
  </div>
  {{end}}
</div>
{{end}}
<script id="key-actions">
(function () {
  var csrfInput = document.getElementById('keys-csrf');
  function csrfValue() { return csrfInput ? csrfInput.value : ''; }
  function setStatus(element, message, ok) {
    if (!element) return;
    element.textContent = message;
    element.className = 'reveal-status ' + (ok ? 'ok' : 'err');
  }
  function manualCopyFallback(textarea, status, label) {
    if (textarea) {
      textarea.focus();
      textarea.select();
    }
    setStatus(status, '复制失败：已为你选中' + label + '，请按系统复制键或长按手动复制。', false);
  }
  function copyFrom(textarea, status, label) {
    if (!textarea || !textarea.value) {
      setStatus(status, '当前没有可复制的' + label + '。', false);
      return;
    }
    var value = textarea.value;
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(value).then(function () {
        setStatus(status, label + '已复制到剪贴板。', true);
      }, function () {
        manualCopyFallback(textarea, status, label);
      });
      return;
    }
    textarea.focus();
    textarea.select();
    var copied = false;
    try { copied = document.execCommand('copy'); } catch (error) { copied = false; }
    if (copied) {
      setStatus(status, label + '已复制到剪贴板。', true);
    } else {
      manualCopyFallback(textarea, status, label);
    }
  }
  function wireCopy(block) {
    var status = block.querySelector('.reveal-status');
    var secret = block.querySelector('.reveal-secret');
    var payload = block.querySelector('.reveal-payload');
    var copySecret = block.querySelector('.copy-secret');
    var copyPayload = block.querySelector('.copy-payload');
    if (copySecret && !copySecret.dataset.wired) {
      copySecret.dataset.wired = '1';
      copySecret.addEventListener('click', function () { copyFrom(secret, status, '配对密钥'); });
    }
    if (copyPayload && !copyPayload.dataset.wired) {
      copyPayload.dataset.wired = '1';
      copyPayload.addEventListener('click', function () { copyFrom(payload, status, '配对内容'); });
    }
    wireHide(block);
  }
  function wireHide(block) {
    var hideButton = block.querySelector('.hide-button');
    if (!hideButton || hideButton.dataset.wired) return;
    hideButton.dataset.wired = '1';
    hideButton.addEventListener('click', function () {
      var fields = block.querySelector('.reveal-fields');
      if (fields) fields.hidden = true;
      var secret = block.querySelector('.reveal-secret');
      var payload = block.querySelector('.reveal-payload');
      if (secret) secret.value = '';
      if (payload) payload.value = '';
      var qr = block.querySelector('.reveal-qr');
      if (qr) { qr.hidden = true; qr.removeAttribute('src'); }
      var reveal = block.querySelector('.reveal-button');
      if (reveal) reveal.hidden = false;
      setStatus(block.querySelector('.reveal-status'), '', true);
    });
  }
  document.querySelectorAll('.key-block').forEach(function (block) {
    wireCopy(block);
    var button = block.querySelector('.reveal-button');
    if (!button) return;
    var status = block.querySelector('.reveal-status');
    button.addEventListener('click', function () {
      button.disabled = true;
      setStatus(status, '正在读取…', true);
      fetch(button.getAttribute('data-reveal-url'), {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: 'csrf=' + encodeURIComponent(csrfValue())
      }).then(function (response) {
        return response.json().catch(function () { return {}; }).then(function (data) {
          return { status: response.status, data: data };
        });
      }).then(function (result) {
        if (result.status !== 200) {
          setStatus(status, result.data.message || '显示失败，请稍后重试。', false);
          return;
        }
        var fields = block.querySelector('.reveal-fields');
        if (fields) fields.hidden = false;
        block.querySelector('.reveal-secret').value = result.data.enrollmentKey || '';
        block.querySelector('.reveal-payload').value = result.data.pairingPayload || '';
        var qr = block.querySelector('.reveal-qr');
        if (qr && result.data.qrDataUri) { qr.src = result.data.qrDataUri; qr.hidden = false; }
        button.hidden = true;
        wireCopy(block);
        setStatus(status, '已显示配对密钥与配对内容。', true);
      }).catch(function () {
        setStatus(status, '读取失败，请刷新后重试。', false);
      }).finally(function () { button.disabled = false; });
    });
  });
})();
</script>
{{end}}

{{define "devices"}}
<h1>设备</h1>
<div class="card"><div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
<tr><th>设备</th><th>密钥</th><th>线路数</th><th>推送</th><th>注册时间</th><th>状态</th><th></th></tr>
{{range .Devices}}<tr>
<td>{{if .Device.Name}}{{.Device.Name}}{{else}}未命名设备{{end}} <span class="muted mono">{{.Device.ID}}</span></td>
<td>{{if .KeyName}}{{.KeyName}}{{else}}密钥已删除{{end}} <span class="muted mono">{{.Device.KeyID}}</span></td>
<td>{{.LineCount}}</td>
<td>{{if .PushRegistered}}<span class="pill ok">已注册</span>{{else}}<span class="pill">未注册</span>{{end}}</td>
<td>{{ts .Device.CreatedAt.UnixMilli}}</td>
<td>{{if .Device.RevokedAt}}<span class="pill err">已撤销</span>{{else}}<span class="pill ok">有效</span>{{end}}</td>
<td class="actions">
{{if not .Device.RevokedAt}}
<form class="inline" method="post" action="/console/devices/{{.Device.ID}}/revoke" onsubmit="return confirm('让这台设备下线？')"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="danger" type="submit">下线</button></form>
{{else}}
<form class="inline" method="post" action="/console/devices/{{.Device.ID}}/allow-reenroll"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="ghost" type="submit">允许重新注册</button></form>
{{end}}
</td>
</tr>{{end}}
{{if not .Devices}}<tr><td colspan="7" class="muted">还没有设备注册</td></tr>{{end}}
</table></div></div>
{{end}}

{{define "calls"}}
<h1>通话</h1>
<div class="card"><div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
<tr><th>通话</th><th>线路</th><th>号码</th><th>方向</th><th>状态</th><th>保持</th><th>开始</th><th>连接</th><th>结束原因</th><th></th></tr>
{{range .Calls}}<tr>
<td class="mono">{{.ID}}</td><td>{{if .LineName}}{{.LineName}}{{else}}{{lineName $.LineNames .LineID}}{{end}}</td><td class="mono">{{.Peer}}</td><td>{{directionLabel .Direction}}</td>
<td>{{stateLabel .State}}</td><td>{{if .Held}}是{{else}}—{{end}}</td>
<td>{{ts .StartedAt}}</td><td>{{if .ConnectedAt}}{{ts .ConnectedAt}}{{else}}—{{end}}</td>
<td class="muted">{{endReasonLabel .EndReason}}</td>
<td class="actions">
{{if .EndedAt}}<span class="muted">已结束</span>{{else}}
<form class="inline" method="post" action="/console/calls/{{.ID}}/hangup"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">挂断</button></form>
{{if eq .State "incoming_ringing"}}<form class="inline" method="post" action="/console/calls/{{.ID}}/reject"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="danger" type="submit">拒接</button></form>{{end}}
{{end}}
</td></tr>{{end}}
{{if not .Calls}}<tr><td colspan="10" class="muted">没有通话记录</td></tr>{{end}}
</table></div></div>
{{end}}

{{define "conferences"}}
<h1>会议</h1>
{{range .Conferences}}{{$conf := .}}
<div class="card">
  <div class="row" style="justify-content:space-between">
    <div><strong>{{if .HostName}}{{.HostName}}{{else}}主持人设备{{end}}</strong> <span class="muted mono">{{.ID}}</span></div>
    <div>{{if .Ended}}<span class="pill">已结束</span>{{else}}<span class="pill ok">{{conferenceStateLabel .State}}</span>{{end}}</div>
  </div>
  <p class="muted">主持人设备 {{if .HostName}}{{.HostName}}{{else}}<span class="mono">{{.HostDeviceID}}</span>{{end}} · 创建 {{ts .CreatedAt}}{{if .GraceDeadline}} · 重连截止 {{ts .GraceDeadline}}{{end}}</p>
  <div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
    <tr><th>通话</th><th>线路</th><th>号码</th><th>方向</th><th>保持</th><th></th></tr>
    {{range .Legs}}<tr>
      <td class="mono">{{.ID}}</td>
      <td>{{if .LineName}}{{.LineName}}{{else}}{{lineName $.LineNames .LineID}}{{end}}</td>
      <td class="mono">{{.Peer}}</td>
      <td>{{directionLabel .Direction}}</td>
      <td>{{if .Held}}是{{else}}—{{end}}</td>
      <td>{{if not $conf.Ended}}<form class="inline" method="post" action="/console/conferences/{{$conf.ID}}/legs/{{.ID}}/hangup"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="ghost" type="submit">移除</button></form>{{end}}</td>
    </tr>{{end}}
  </table></div>
  {{if not .Ended}}<form method="post" action="/console/conferences/{{.ID}}/close" onsubmit="return confirm('结束会议并挂断全部成员？')"><input type="hidden" name="csrf" value="{{$.CSRF}}"><div style="margin-top:12px"><button class="danger" type="submit">结束会议</button></div></form>{{end}}
</div>
{{end}}
{{if not .Conferences}}<div class="card muted">当前没有会议。</div>{{end}}
{{end}}

{{define "voicemails"}}
<h1>语音留言</h1>
<p class="muted">留言存储：{{if .VoicemailStorage.Mounted}}<span class="pill ok">SD 可用</span> · 空闲 {{bytes .VoicemailStorage.FreeBytes}}{{else}}<span class="pill err">不可用，留言暂停</span>{{end}} · 已用 {{bytes .VoicemailBytes}}</p>
<div class="card"><div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
<tr><th>线路</th><th>号码</th><th>时长</th><th>大小</th><th>时间</th><th>到期</th><th>播放</th><th></th></tr>
{{range .Voicemails}}<tr>
<td>{{if .LineName}}{{.LineName}}{{else}}{{lineName $.LineNames .LineID}}{{end}}</td><td class="mono">{{.Peer}}</td>
<td>{{msduration .DurationMs}}</td><td>{{bytes .SizeBytes}}</td><td>{{ts .CreatedAt}}</td><td>{{ts .ExpiresAt}}</td>
<td>{{if eq .State "ready"}}<audio controls preload="none" src="/console/voicemails/{{.ID}}/audio" style="height:28px"></audio> <span class="muted">可播放</span>{{else}}{{stateLabel .State}}{{end}}</td>
<td><form class="inline" method="post" action="/console/voicemails/{{.ID}}/delete" onsubmit="return confirm('删除该留言？')"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="danger" type="submit">删除</button></form></td>
</tr>{{end}}
{{if not .Voicemails}}<tr><td colspan="8" class="muted">还没有留言</td></tr>{{end}}
</table></div></div>
{{end}}

{{define "settings"}}
<h1>设置</h1>
<form method="post" action="/console/settings">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<div class="card">
  <h2 style="margin-top:0">通话与并发</h2>
  <div class="grid">
    <div><label>最大并发通话</label><input type="number" name="max_concurrent_calls" min="1" max="32" value="{{index .Settings "max_concurrent_calls"}}"></div>
    <div><label>主叫断开重连宽限（秒）</label><input type="number" name="disconnect_grace_seconds" min="5" max="600" value="{{index .Settings "disconnect_grace_seconds"}}"></div>
  </div>
</div>
<div class="card">
  <h2 style="margin-top:0">语音留言</h2>
  <div class="checkline"><label><input type="checkbox" name="voicemail_enabled" {{if eq (index .Settings "voicemail_enabled") "1"}}checked{{end}}> 启用语音留言</label></div>
  <div class="grid">
    <div><label>无人接听转入秒数</label><input type="number" name="voicemail_ring_seconds" min="5" max="300" value="{{index .Settings "voicemail_ring_seconds"}}"></div>
    <div><label>无在线设备转入秒数</label><input type="number" name="voicemail_offline_seconds" min="2" max="300" value="{{index .Settings "voicemail_offline_seconds"}}"></div>
    <div><label>最长留言秒数</label><input type="number" name="voicemail_max_seconds" min="10" max="600" value="{{index .Settings "voicemail_max_seconds"}}"></div>
    <div><label>留存天数</label><input type="number" name="voicemail_retention_days" min="1" max="3650" value="{{index .Settings "voicemail_retention_days"}}"></div>
    <div><label>容量上限 (MB)</label><input type="number" name="voicemail_max_bytes_mb" min="10" max="102400" value="{{index .Settings "voicemail_max_bytes_mb"}}"></div>
    <div><label>最小空闲空间 (MB)</label><input type="number" name="voicemail_min_free_bytes_mb" min="16" max="102400" value="{{index .Settings "voicemail_min_free_bytes_mb"}}"></div>
  </div>
</div>
<div style="margin:6px 0 30px"><button type="submit">保存设置</button></div>
</form>
{{end}}

{{define "diagnostics"}}
<h1>诊断</h1>
<div class="card">
  <h2 style="margin-top:0">网关身份</h2>
  <p class="mono">ID {{.GatewayID}}<br>指纹 {{.Fingerprint}}<br>公钥 {{.PublicKey}}</p>
  <p class="muted">推送：{{if .PushReady}}已配置{{else}}未配置{{end}} · 网络中继：{{.TURNPolicy}}</p>
  <p class="muted">留言存储：{{if .VoicemailStorage.Mounted}}SD {{.VoicemailStorage.Device}} ({{.VoicemailStorage.FSType}})，空闲 {{bytes .VoicemailStorage.FreeBytes}}{{else}}不可用（{{.VoicemailStorage.Error}}）{{end}}</p>
</div>
<div class="card">
  <h2 style="margin-top:0">线路健康与恢复</h2>
  <div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table>
    <tr><th>线路</th><th>在线</th><th>SIM</th><th>注册</th><th>语音</th><th>短信</th><th>错误</th><th></th></tr>
    {{range .Lines}}<tr>
      <td>{{.Name}} <span class="muted mono">{{.ID}}</span></td>
      <td>{{if .Online}}<span class="pill ok">是</span>{{else}}<span class="pill err">否</span>{{end}}</td>
      <td>{{.SIM}}</td><td>{{.Registration}}</td><td>{{.Voice}}</td><td>{{.SMS}}</td>
      <td class="muted">{{.LastError}}</td>
      <td><form class="inline" method="post" action="/console/diagnostics/recover/{{.ID}}"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="ghost" type="submit">恢复探测</button></form></td>
    </tr>{{end}}
  </table></div>
</div>
<div class="card">
  <h2 style="margin-top:0">最近事件</h2>
  <div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table><tr><th>序号</th><th>类型</th><th>线路</th><th>时间</th></tr>
  {{range .Events}}<tr><td>{{.seq}}</td><td class="mono">{{.type}}</td><td class="mono">{{.lineId}}</td><td>{{ts .createdAt}}</td></tr>{{end}}
  </table></div>
</div>
<div class="card">
  <h2 style="margin-top:0">操作审计</h2>
  <div class="table-scroll" tabindex="0" role="region" aria-label="可横向滚动的数据表"><table><tr><th>时间</th><th>操作者</th><th>动作</th><th>对象</th><th>结果</th></tr>
  {{range .Audit}}<tr><td>{{ts .CreatedAt.UnixMilli}}</td><td>{{.Actor}}</td><td class="mono">{{.Action}}</td><td class="mono">{{.Target}}</td><td>{{.Result}}</td></tr>{{end}}
  </table></div>
</div>
{{end}}
`
