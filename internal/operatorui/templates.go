package operatorui

const layoutSrc = `{{define "layout"}}<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>RecallDock · {{.Title}}</title>
<link rel="icon" type="image/png" href="/assets/logo.png">
<style>
:root{
  color-scheme:light dark;
  --bg:#f5f6f8; --bg-accent:#eef1ff; --surface:#ffffff; --surface-2:#f8f9fb; --surface-3:#f1f2f6;
  --fg:#0f172a; --fg-soft:#334155; --muted:#64748b; --faint:#94a3b8;
  --line:#e4e7ec; --line-strong:#cfd5df;
  --accent:#4f46e5; --accent-hover:#4338ca; --accent-soft:#eef2ff; --accent-line:#c7d2fe;
  --ok:#047857; --ok-soft:#ecfdf5; --warn:#b45309; --warn-soft:#fffbeb; --err:#b91c1c; --err-soft:#fef2f2;
  --radius:14px; --radius-sm:9px;
  --shadow:0 1px 2px rgba(15,23,42,.04),0 4px 14px -8px rgba(15,23,42,.12);
  --mono:ui-monospace,SFMono-Regular,"JetBrains Mono",Menlo,Consolas,monospace;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#0b0d12; --bg-accent:#10121c; --surface:#14171f; --surface-2:#181c26; --surface-3:#1e2330;
    --fg:#e8eaf0; --fg-soft:#cbd2e0; --muted:#98a2b3; --faint:#6b7688;
    --line:#252a36; --line-strong:#333a49;
    --accent:#8b93ff; --accent-hover:#aab1ff; --accent-soft:#1b1f3a; --accent-line:#343a68;
    --ok:#34d399; --ok-soft:#062b25; --warn:#fbbf24; --warn-soft:#2a2108; --err:#f87171; --err-soft:#2b1012;
    --shadow:0 1px 2px rgba(0,0,0,.5),0 10px 30px -18px rgba(0,0,0,.8);
  }
}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%;scroll-behavior:smooth}
body{
  margin:0;color:var(--fg);background:var(--bg);
  font:15px/1.6 ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans CJK SC","PingFang SC",sans-serif;
  background-image:radial-gradient(1100px 420px at 50% -180px,color-mix(in srgb,var(--accent) 12%,transparent),transparent 70%);
  min-height:100vh;display:flex;flex-direction:column;
}
a{color:var(--accent);text-decoration:none;text-underline-offset:3px}
a:hover{text-decoration:underline}
:focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:4px}
.skip{position:absolute;left:-9999px;top:0;background:var(--surface);padding:10px 14px;z-index:50}
.skip:focus{left:12px;top:12px}
header{
  position:sticky;top:0;z-index:20;display:flex;align-items:center;gap:18px;flex-wrap:wrap;
  padding:10px clamp(16px,3vw,28px);border-bottom:1px solid var(--line);
  background:color-mix(in srgb,var(--bg) 86%,transparent);backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);
}
.brand{display:flex;align-items:center;gap:9px;font-weight:680;font-size:15px;color:var(--fg);letter-spacing:.1px;flex:none}
.brand:hover{text-decoration:none}
.brand .logo{width:27px;height:27px;border-radius:9px;display:block;box-shadow:var(--shadow)}
.ver{color:var(--muted);font-size:11px;border:1px solid var(--line);border-radius:999px;padding:1px 8px;font-variant-numeric:tabular-nums}
nav{display:flex;gap:2px;flex-wrap:wrap;min-width:0}
nav a{
  display:inline-flex;align-items:center;gap:6px;color:var(--muted);font-size:13px;padding:6px 10px;border-radius:999px;
  transition:background .15s,color .15s;white-space:nowrap;
}
nav a .ico{opacity:.7;font-size:12px;line-height:1}
nav a:hover{color:var(--fg);background:var(--surface-3);text-decoration:none}
nav a[aria-current=page]{color:var(--accent);background:var(--accent-soft);font-weight:600}
nav a[aria-current=page] .ico{opacity:1}
nav a .count{display:inline-block;min-width:17px;text-align:center;margin-left:2px;padding:0 5px;border-radius:999px;background:var(--warn-soft);color:var(--warn);font-size:11px;font-weight:700}
.headtools{margin-left:auto;display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.scope-picker{display:flex;gap:7px;align-items:center;margin:0}
.scope-picker label{font-size:11px;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);font-weight:650;white-space:nowrap;flex:none}
.scope-picker select{max-width:230px}
main{width:100%;max-width:1140px;margin:0 auto;padding:clamp(18px,3vw,32px) clamp(16px,3vw,28px) 72px;flex:1}
.pagehead{display:flex;align-items:flex-end;gap:18px;flex-wrap:wrap;margin:0 0 20px}
.pagehead .grow{flex:1;min-width:240px}
h1{font-size:clamp(22px,3vw,27px);line-height:1.22;letter-spacing:-.02em;margin:0;font-weight:700}
.pagehead .sub{color:var(--muted);font-size:13.5px;margin:6px 0 0;max-width:76ch}
.pagehead .sub .mono{color:var(--fg-soft)}
h2{font-size:12px;letter-spacing:.09em;color:var(--muted);text-transform:uppercase;margin:30px 0 12px;font-weight:700}
h2 .after{text-transform:none;letter-spacing:0;font-weight:500;margin-left:8px;color:var(--faint)}
p{max-width:82ch}
.muted{color:var(--muted)}
.faint{color:var(--faint)}
.mono{font-family:var(--mono);font-size:12px;letter-spacing:-.01em}
.notice,.error{
  display:flex;gap:10px;align-items:flex-start;padding:12px 15px;border-radius:var(--radius-sm);margin:0 0 20px;
  font-size:14px;border:1px solid;line-height:1.55;
}
.notice{background:var(--accent-soft);border-color:var(--accent-line);color:var(--fg-soft)}
.error{background:var(--err-soft);border-color:color-mix(in srgb,var(--err) 35%,transparent);color:var(--err)}
.notice::before{content:"◆";color:var(--accent);font-size:11px;line-height:1.9}
.error::before{content:"!";display:inline-grid;place-items:center;width:17px;height:17px;border-radius:50%;background:var(--err);color:#fff;font-size:11px;font-weight:800;line-height:1;flex:none;margin-top:2px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(168px,1fr));gap:12px;margin:0 0 6px}
.card{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);padding:15px 17px;box-shadow:var(--shadow)}
.card.info{background:linear-gradient(180deg,var(--surface-2),var(--surface))}
.card .k{color:var(--muted);font-size:11px;letter-spacing:.08em;text-transform:uppercase;font-weight:700}
.card .v{font-size:27px;line-height:1.2;font-weight:700;margin-top:3px;font-variant-numeric:tabular-nums;letter-spacing:-.01em}
.card .v.small{font-size:17px;white-space:nowrap;letter-spacing:0}
.card .sub{font-size:12px;color:var(--muted);margin-top:2px}
.stat{position:relative;overflow:hidden}
.stat::after{content:"";position:absolute;left:0;top:0;bottom:0;width:3px;background:var(--dot,var(--accent));opacity:.85}
.stat.one{--dot:var(--accent)} .stat.two{--dot:var(--ok)} .stat.three{--dot:var(--warn)} .stat.four{--dot:#0ea5e9} .stat.five{--dot:var(--faint)}
.table-wrap{overflow-x:auto;border-radius:var(--radius);border:1px solid var(--line);background:var(--surface);box-shadow:var(--shadow)}
table{width:100%;border-collapse:collapse;background:var(--surface);min-width:520px}
th,td{text-align:left;padding:11px 14px;border-bottom:1px solid var(--line);vertical-align:top}
th{font-size:11px;letter-spacing:.07em;text-transform:uppercase;color:var(--muted);font-weight:700;background:var(--surface-2);white-space:nowrap;position:sticky;top:0}
tbody tr:last-child td{border-bottom:none}
tbody tr:hover td{background:color-mix(in srgb,var(--accent) 4%,transparent)}
td.nowrap,th.nowrap{white-space:nowrap}
td.id{font-family:var(--mono);color:var(--faint);font-size:12px;white-space:nowrap}
.cell-title{font-weight:600;color:var(--fg)}
.cell-title a{color:var(--fg)}
.cell-title a:hover{color:var(--accent)}
.cell-sub{color:var(--muted);font-size:12.5px;margin-top:2px;max-width:70ch;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.ellip{display:inline-block;max-width:185px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;vertical-align:bottom}
mark.hl{background:color-mix(in srgb,var(--accent) 24%,transparent);color:inherit;border-radius:3px;padding:0 2px}
.badge{display:inline-flex;align-items:center;gap:4px;padding:1px 8px;border-radius:999px;border:1px solid var(--line);font-size:11px;font-weight:600;color:var(--muted);background:var(--surface-2);white-space:nowrap}
.badge.warn{color:var(--warn);border-color:color-mix(in srgb,var(--warn) 40%,transparent);background:var(--warn-soft)}
.badge.err{color:var(--err);border-color:color-mix(in srgb,var(--err) 40%,transparent);background:var(--err-soft)}
.badge.ok{color:var(--ok);border-color:color-mix(in srgb,var(--ok) 40%,transparent);background:var(--ok-soft)}
.badge.accent{color:var(--accent);border-color:var(--accent-line);background:var(--accent-soft)}
form.inline{display:inline-flex;gap:8px;align-items:center;flex-wrap:wrap;margin:0}
.filters{display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);padding:14px;margin:0 0 20px;box-shadow:var(--shadow)}
.field{display:flex;flex-direction:column;gap:4px;min-width:0}
.field label{font-size:11px;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);font-weight:700}
.field.grow{flex:1;min-width:220px}
input[type=text],input[type=search],select,textarea{
  padding:8px 11px;border:1px solid var(--line-strong);border-radius:var(--radius-sm);background:var(--surface);color:var(--fg);
  font:inherit;font-size:14px;transition:border-color .15s,box-shadow .15s;width:100%;
}
input::placeholder,textarea::placeholder{color:var(--faint)}
input:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 16%,transparent)}
textarea{min-height:90px;resize:vertical}
button,.btn{
  display:inline-flex;align-items:center;justify-content:center;gap:6px;padding:8px 14px;border-radius:var(--radius-sm);
  border:1px solid var(--accent);background:var(--accent);color:#fff;cursor:pointer;font:inherit;font-weight:600;font-size:14px;
  transition:background .15s,border-color .15s,transform .1s;white-space:nowrap;text-decoration:none;
}
button:hover,.btn:hover{background:var(--accent-hover);border-color:var(--accent-hover);text-decoration:none;color:#fff}
button:active,.btn:active{transform:translateY(1px)}
button.ghost,.btn.ghost{background:transparent;color:var(--accent)}
button.ghost:hover,.btn.ghost:hover{background:var(--accent-soft);color:var(--accent)}
button.quiet,.btn.quiet{background:transparent;border-color:var(--line-strong);color:var(--muted);font-weight:500}
button.quiet:hover,.btn.quiet:hover{border-color:var(--line-strong);color:var(--fg);background:var(--surface-3)}
button.danger{background:var(--err);border-color:var(--err)}
button.danger:hover{background:color-mix(in srgb,var(--err) 82%,black);border-color:transparent}
button.small,.btn.small{padding:5px 10px;font-size:12.5px;border-radius:7px}
.actions{display:flex;gap:10px;align-items:center;margin:16px 0 4px;flex-wrap:wrap}
.actionbar{display:flex;gap:10px;align-items:center;flex-wrap:wrap;padding:13px 15px;background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);margin:16px 0}
.actionbar .field{flex:1;min-width:220px}
.pager{display:flex;gap:10px;align-items:center;margin-top:16px;flex-wrap:wrap}
.pager .spacer{flex:1}
.empty{border:1px dashed var(--line-strong);border-radius:var(--radius);padding:34px 22px;text-align:center;color:var(--muted);background:var(--surface-2);font-size:14px}
.empty .glyph{display:block;font-size:22px;color:var(--faint);margin-bottom:6px}
.empty code{font-family:var(--mono);font-size:12.5px;background:var(--surface-3);border:1px solid var(--line);border-radius:6px;padding:1px 6px;color:var(--fg-soft)}
pre.content{
  white-space:pre-wrap;word-break:break-word;background:var(--surface);border:1px solid var(--line);
  border-radius:var(--radius);padding:18px 20px;max-height:60vh;overflow:auto;line-height:1.7;box-shadow:var(--shadow);
  font:14px/1.7 var(--mono);margin:0;
}
.meta{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:8px 0 0}
.tech{margin:18px 0 0;border:1px solid var(--line);border-radius:var(--radius-sm);background:var(--surface-2);padding:10px 14px}
.tech summary{cursor:pointer;color:var(--muted);font-size:13px;font-weight:600}
.tech dl{display:grid;grid-template-columns:auto 1fr;gap:6px 14px;margin:12px 0 4px;font-size:12.5px}
.tech dt{color:var(--muted)}
.tech dd{margin:0;font-family:var(--mono);word-break:break-all}
.group{margin:0 0 26px}
.group-head{display:flex;align-items:center;gap:10px;margin:0 0 10px}
.group-head h3{font-size:15px;margin:0;font-weight:650}
.status-hero{display:flex;align-items:center;gap:18px;flex-wrap:wrap;padding:18px 20px;border:1px solid var(--line);border-left:4px solid var(--ok);border-radius:var(--radius);background:var(--surface);box-shadow:var(--shadow);margin:0 0 20px}
.status-hero.warn{border-left-color:var(--warn)}
.status-hero.err{border-left-color:var(--err)}
.status-hero .big{font-size:22px;font-weight:700;letter-spacing:-.01em}
.status-hero .counts{display:flex;gap:16px;flex-wrap:wrap;color:var(--muted);font-size:13px;font-variant-numeric:tabular-nums}
.check-ok{color:var(--faint)}
footer.site{border-top:1px solid var(--line);padding:18px clamp(16px,3vw,28px);color:var(--faint);font-size:12px;display:flex;gap:14px;flex-wrap:wrap;align-items:center;max-width:1140px;margin:0 auto;width:100%}
footer.site .spacer{flex:1}
@media (max-width:960px){.scope-picker label{display:none}}
@media (max-width:820px){
  header{gap:10px;padding:10px 14px}
  nav{order:3;width:100%;flex-wrap:wrap}
  .headtools{margin-left:auto}
  .scope-picker select{max-width:170px}
  main{padding:18px 14px 56px}
  .pagehead{gap:12px}
  .grid{grid-template-columns:repeat(auto-fit,minmax(140px,1fr))}
  .filters .field.grow{min-width:100%}
  .hide-sm{display:none}
}
@media (max-width:620px){.hide-xs{display:none}}
@media (prefers-reduced-motion:reduce){*{transition:none!important;scroll-behavior:auto!important}}
</style>
</head>
<body>
<a class="skip" href="#main">跳到主要内容</a>
<header>
  <a class="brand" href="/"><img class="logo" src="/assets/logo.png" alt=""><span>RecallDock</span><span class="ver">{{.Version}}</span></a>
  <nav aria-label="主导航">
    <a href="/"{{if eq .Page "dashboard"}} aria-current="page"{{end}}><span class="ico">◈</span>概览</a>
    <a href="/search"{{if eq .Page "search"}} aria-current="page"{{end}}><span class="ico">⌕</span>搜索</a>
    <a href="/similar"{{if or (eq .Page "similar") (eq .Page "refine") (eq .Page "refineresult")}} aria-current="page"{{end}}><span class="ico">⧉</span>相似</a>
    <a href="/candidates"{{if eq .Page "candidates"}} aria-current="page"{{end}}><span class="ico">✓</span>候选{{if gt .Pending 0}}<span class="count">{{.Pending}}</span>{{end}}</a>
    <a href="/projects"{{if eq .Page "projects"}} aria-current="page"{{end}}><span class="ico">▤</span>项目</a>
    <a href="/doctor"{{if eq .Page "doctor"}} aria-current="page"{{end}}><span class="ico">✚</span>诊断</a>
    <a href="/backups"{{if eq .Page "backups"}} aria-current="page"{{end}}><span class="ico">⛁</span>备份</a>
    <a href="/conflicts"{{if eq .Page "conflicts"}} aria-current="page"{{end}}><span class="ico">⚠</span>冲突</a>
    <a href="/archived"{{if eq .Page "archived"}} aria-current="page"{{end}}><span class="ico">⌸</span>已归档</a>
  </nav>
  <div class="headtools">
    {{if .ReadOnly}}<span class="badge warn" title="以 --read-only 启动：只浏览，不写入">只读</span>{{end}}
    <form method="get" action="/project" class="scope-picker">
      <label for="scope">范围</label>
      <select id="scope" name="value">{{range .ScopeOptions}}<option value="{{.Value}}"{{if .Selected}} selected{{end}}>{{.Label}}</option>{{end}}</select>
      <button type="submit" class="ghost small">切换</button>
    </form>
  </div>
</header>
<main id="main">
{{if .Notice}}<div class="notice">{{.Notice}}</div>{{end}}
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if eq .Page "dashboard"}}{{template "dashboard" .}}{{end}}
{{if eq .Page "search"}}{{template "search" .}}{{end}}
{{if eq .Page "memory"}}{{template "memory" .}}{{end}}
{{if eq .Page "candidates"}}{{template "candidates" .}}{{end}}
{{if eq .Page "batchresult"}}{{template "batchresult" .}}{{end}}
{{if eq .Page "projects"}}{{template "projects" .}}{{end}}
{{if eq .Page "doctor"}}{{template "doctor" .}}{{end}}
{{if eq .Page "backups"}}{{template "backups" .}}{{end}}
{{if eq .Page "conflicts"}}{{template "conflicts" .}}{{end}}
{{if eq .Page "archived"}}{{template "archived" .}}{{end}}
{{if eq .Page "edit"}}{{template "edit" .}}{{end}}
{{if eq .Page "similar"}}{{template "similar" .}}{{end}}
{{if eq .Page "refine"}}{{template "refine" .}}{{end}}
{{if eq .Page "refineresult"}}{{template "refineresult" .}}{{end}}
{{if eq .Page "error"}}{{template "errorpage" .}}{{end}}
{{if .TruncatedNote}}<div class="notice" style="margin-top:18px">{{.TruncatedNote}}</div>{{end}}
</main>
<footer class="site">
  <span>RecallDock {{.Version}}</span>
  <span>·</span>
  <span>本机回环界面，数据不离开这台机器</span>
  <span class="spacer"></span>
  {{if .Identity.StoreID}}<span class="mono">store {{.Identity.StoreID}}</span>{{end}}
</footer>
</body>
</html>{{end}}

{{define "errorpage"}}
<div class="pagehead"><div class="grow"><h1>{{.Title}}</h1>
<p class="sub">请求没有完成。原因见上方提示；修正好地址或状态后可以重试。</p></div></div>
<div class="actions"><a class="btn ghost small" href="/">返回概览</a><a class="btn quiet small" href="/search">去搜索</a><a class="btn quiet small" href="/doctor">跑诊断</a></div>
{{end}}

{{define "dashboard"}}
<div class="pagehead">
  <div class="grow">
    <h1>概览</h1>
    <p class="sub">当前范围：<strong>{{.ProjectLabel}}</strong>{{if .ProjectHint}} · 未绑定目录提示 <span class="mono">{{.ProjectHint}}</span>{{end}}</p>
  </div>
  <form method="get" action="/search" class="filters" style="margin:0;padding:8px;gap:6px;flex:1;min-width:260px;max-width:420px;align-items:center">
    <input type="search" name="q" placeholder="搜索标题或正文…" aria-label="搜索记忆" style="flex:1">
    <button type="submit">搜索</button>
  </form>
</div>
<div class="grid">
  <div class="card stat one"><div class="k">长期记忆</div><div class="v">{{.Stats.TotalObservations}}</div><div class="sub">当前范围</div></div>
  <div class="card stat two"><div class="k">会话</div><div class="v">{{.Stats.TotalSessions}}</div><div class="sub">已记录</div></div>
  <div class="card stat three"><div class="k">待审候选</div><div class="v">{{.Pending}}</div><div class="sub"><a href="/candidates">去审核</a></div></div>
  <div class="card stat four"><div class="k">项目</div><div class="v">{{len .Stats.Projects}}</div><div class="sub"><a href="/projects">看绑定</a></div></div>
  <div class="card stat five"><div class="k">上次备份</div><div class="v small">{{if .BackupTime}}{{.BackupTime}}{{else}}从未{{end}}</div><div class="sub">{{if .BackupCount}}共 {{.BackupCount}} 份 · 最新 {{.BackupSize}}{{else}}建议先备份{{end}}</div></div>
</div>
<p class="faint mono" style="margin:10px 0 0">store {{.Identity.StoreID}} · {{.Identity.DatabasePath}}</p>
<h2>最近记忆<span class="after">按创建时间倒序</span></h2>
{{if .Recent}}
<div class="table-wrap">
<table>
<thead><tr><th class="hide-sm">#</th><th>标题</th><th>类型</th><th class="hide-xs">项目</th><th>时间</th></tr></thead>
<tbody>
{{range .Recent}}<tr>
  <td class="id hide-sm">{{.ID}}</td>
  <td><div class="cell-title"><a href="/memory/{{.ID}}">{{.Title}}</a></div>{{if .Abstract}}<div class="cell-sub">{{clip .Abstract 110}}</div>{{end}}</td>
  <td><span class="badge">{{.Type}}</span></td>
  <td class="hide-xs"><span class="mono muted ellip" title="{{project .Project}}">{{project .Project}}</span></td>
  <td class="muted nowrap" title="{{fmtTime .CreatedAt}}">{{ago .CreatedAt}}</td>
</tr>{{end}}
</tbody>
</table>
</div>
<div class="actions"><a class="btn ghost small" href="/search">搜索全部 →</a><a class="btn quiet small" href="/archived">已归档</a></div>
{{else}}<div class="empty"><span class="glyph">◈</span>还没有记录。先用 <code>recalldock save "标题" "内容"</code> 写入，或让 AI 提交候选。</div>{{end}}
{{end}}

{{define "search"}}
<div class="pagehead">
  <div class="grow">
    <h1>搜索</h1>
    <p class="sub">本地 FTS5 全文检索，支持中文与英文；时间格式 <span class="mono">YYYY-MM-DD</span> 或 <span class="mono">YYYY-MM-DD HH:MM:SS</span>。</p>
  </div>
</div>
<form method="get" action="/search" class="filters">
  <div class="field grow"><label for="q">关键词</label><input id="q" type="text" name="q" value="{{.Query}}" placeholder="例如：发布流程、WAL、candidate" autofocus></div>
  <div class="field"><label for="project">项目筛选</label>
    <select id="project" name="project">
      <option value="*"{{if .AllProjects}} selected{{end}}>全部项目</option>
      {{range .ProjectOptions}}<option value="{{.Value}}"{{if .Selected}} selected{{end}}>{{.Label}}</option>{{end}}
    </select>
  </div>
  <div class="field"><label for="since">起始</label><input id="since" type="text" name="since" value="{{.Since}}" placeholder="2026-11-01" size="14"></div>
  <div class="field"><label for="until">结束</label><input id="until" type="text" name="until" value="{{.Until}}" placeholder="2026-11-30" size="14"></div>
  <div class="field"><label>&nbsp;</label><button type="submit">搜索</button></div>
</form>
{{if .Query}}
<p class="muted" style="margin:0 0 12px">本页 <strong>{{len .Results}}</strong> 条 · 范围：<strong>{{.ProjectLabel}}</strong>{{if .HasNext}} · <span class="badge accent">还有更多</span>{{end}}</p>
{{if .Results}}
<div class="table-wrap">
<table>
<thead><tr><th class="hide-sm">#</th><th>标题</th><th>类型</th><th class="hide-xs">项目</th><th>时间</th></tr></thead>
<tbody>
{{range .Results}}<tr>
  <td class="id hide-sm">{{.ID}}</td>
  <td><div class="cell-title"><a href="/memory/{{.ID}}">{{highlight .Title $.Query}}</a></div>{{if .Abstract}}<div class="cell-sub">{{highlight (clip .Abstract 140) $.Query}}</div>{{end}}</td>
  <td><span class="badge">{{.Type}}</span></td>
  <td class="hide-xs"><span class="mono muted ellip" title="{{project .Project}}">{{project .Project}}</span></td>
  <td class="muted nowrap" title="{{fmtTime .CreatedAt}}">{{ago .CreatedAt}}</td>
</tr>{{end}}
</tbody>
</table>
</div>
{{else}}<div class="empty"><span class="glyph">⌕</span>没有匹配「{{.Query}}」。试试更短的关键词，或把项目筛选改成「全部项目」。</div>{{end}}
{{if or .HasPrev .HasNext}}<div class="pager">
  {{if .HasPrev}}<a class="btn quiet small" href="{{.PrevURL}}">← 上一页</a>{{end}}
  <span class="muted mono">第 {{.PageNumber}} 页</span>
  {{if .HasNext}}<a class="btn ghost small" href="{{.NextURL}}">下一页 →</a>{{end}}
</div>{{end}}
{{else}}
<div class="empty"><span class="glyph">⌕</span>输入关键词开始搜索；搜索范围跟随右上角的项目选择。</div>
{{end}}
{{end}}

{{define "memory"}}
<div class="pagehead">
  <div class="grow">
    <h1>{{.Obs.Title}}</h1>
    <div class="meta">
      <span class="badge">#{{.Obs.ID}}</span>
      <span class="badge accent">{{.Obs.Type}}</span>
      <span class="badge">{{.Obs.MemoryClass}}</span>
      {{if .IsArchived}}<span class="badge warn">已归档</span>{{end}}
      <span class="badge" title="修订次数">r{{.Obs.RevisionCount}}</span>
      <span class="mono muted">{{project .Obs.Project}}</span>
      <span class="muted" title="{{fmtTime .Obs.CreatedAt}}">创建于 {{ago .Obs.CreatedAt}}</span>
    </div>
  </div>
  <div class="actions" style="margin:0">
    {{if .ReadOnly}}<span class="badge warn">只读模式</span>{{else}}
      <a class="btn ghost small" href="/memory/{{.Obs.ID}}/edit">编辑正文</a>
      {{if .IsArchived}}
      <form method="post" action="/memory/{{.Obs.ID}}/reactivate" class="inline">
        <input type="hidden" name="csrf" value="{{.CSRF}}">
        <input type="hidden" name="expected" value="{{.Digest}}">
        <input type="text" name="reason" placeholder="恢复原因（必填）" required style="width:190px">
        <button type="submit" class="small">恢复</button>
      </form>
      {{else}}
      <form method="post" action="/memory/{{.Obs.ID}}/archive" class="inline">
        <input type="hidden" name="csrf" value="{{.CSRF}}">
        <input type="hidden" name="expected" value="{{.Digest}}">
        <input type="text" name="reason" placeholder="归档原因（必填）" required style="width:190px">
        <button type="submit" class="quiet small">归档</button>
      </form>
      {{end}}
    {{end}}
  </div>
</div>
{{if .IsArchived}}<div class="notice">这条记忆已归档：默认召回不再包含它，但内容与历史都还在，可以随时恢复。</div>{{end}}
<h2>正文</h2>
<pre class="content">{{.Obs.Content}}</pre>
{{if .Obs.Abstract}}<h2>摘要</h2><p>{{.Obs.Abstract}}</p>{{end}}
{{if .Obs.Overview}}<h2>概览</h2><p>{{.Obs.Overview}}</p>{{end}}
<details class="tech">
  <summary>技术细节（快照、身份、时间）</summary>
  <dl>
    <dt>快照摘要</dt><dd>{{.Digest}}</dd>
    <dt>同步 ID</dt><dd>{{.Obs.SyncID}}</dd>
    <dt>项目</dt><dd>{{project .Obs.Project}}</dd>
    <dt>会话</dt><dd>{{.Obs.SessionID}}</dd>
    <dt>创建时间</dt><dd>{{fmtTime .Obs.CreatedAt}}</dd>
    <dt>修订</dt><dd>{{.Obs.RevisionCount}}</dd>
  </dl>
</details>
<h2>历史版本<span class="after">最近 10 条</span></h2>
{{if .History}}
<div class="table-wrap">
<table>
<thead><tr><th>序号</th><th>种类</th><th>标题</th><th>大小</th><th>时间</th></tr></thead>
<tbody>
{{range .History}}<tr><td class="id">{{.Sequence}}</td><td><span class="badge">{{.Kind}}</span></td><td>{{.Title}}</td><td class="muted nowrap">{{bytes .StateBytes}}</td><td class="muted nowrap" title="{{fmtTime .CapturedAt}}">{{ago .CapturedAt}}</td></tr>{{end}}
</tbody>
</table>
</div>
{{else}}<div class="empty">还没有历史版本。</div>{{end}}
{{end}}

{{define "candidates"}}
<div class="pagehead">
  <div class="grow">
    <h1>候选审核</h1>
    <p class="sub">AI 只能提交候选；正式知识由你批准。勾选后填一次理由即可批量处理，每条仍单独校验快照摘要，过期候选会失败而不会覆盖。</p>
  </div>
</div>
{{if .ReadOnly}}<div class="notice">只读模式：这里只展示候选，不提供批准/拒绝。</div>{{end}}
{{if .Candidates}}
{{if .ReadOnly}}
<div class="table-wrap">
<table>
<thead><tr><th>候选</th><th>类别</th><th>项目</th><th>到期</th></tr></thead>
<tbody>
{{range .Candidates}}<tr>
  <td><div class="cell-title">{{.Preview}}</div><div class="mono faint">{{.ID}}</div></td>
  <td><span class="badge accent">{{category .Category}}</span></td>
  <td><span class="mono muted ellip" title="{{.Project}}">{{.Project}}</span></td>
  <td class="muted nowrap" title="{{.ExpiresAt}}">{{ago .ExpiresAt}}</td>
</tr>{{end}}
</tbody>
</table>
</div>
{{else}}
<form method="post" action="/candidates/approve-batch">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<div class="table-wrap">
<table>
<thead><tr><th style="width:34px"><span class="faint">选</span></th><th>候选</th><th>类别</th><th>来源</th><th>项目</th><th class="nowrap">到期</th></tr></thead>
<tbody>
{{range .Candidates}}
<tr>
  <td><input type="checkbox" name="ids" value="{{.ID}}" aria-label="选择候选 {{.ID}}"></td>
  <td><div class="cell-title">{{.Preview}}</div><div class="mono faint">{{.ID}}</div>
      <input type="hidden" name="expected_{{.ID}}" value="{{.Digest}}"></td>
  <td><span class="badge accent" title="{{.Category}}">{{category .Category}}</span></td>
  <td><span class="badge" title="{{.Origin}}">{{origin .Origin}}</span></td>
  <td><span class="mono muted ellip" title="{{.Project}}">{{.Project}}</span></td>
  <td class="muted nowrap" title="{{.ExpiresAt}}">{{ago .ExpiresAt}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
<div class="actionbar">
  <div class="field"><label for="reason">处理理由（必填）</label><input id="reason" type="text" name="reason" placeholder="例如：已在项目里核对来源" required></div>
  <button type="submit">批准所选</button>
  <button type="submit" formaction="/candidates/reject-batch" class="quiet">拒绝所选</button>
</div>
</form>
{{end}}
{{else}}<div class="empty"><span class="glyph">✓</span>没有待审候选。模型提交候选后，会出现在这里。</div>{{end}}
{{end}}

{{define "batchresult"}}
<div class="pagehead"><div class="grow"><h1>批量处理结果</h1>
<p class="sub">批准 {{.BatchApproved}} 条，拒绝 {{.BatchRejected}} 条，失败 {{.BatchFailed}} 条。失败项保持原状，可刷新后重试。</p></div></div>
{{if .BatchResults}}
<div class="table-wrap">
<table><thead><tr><th>候选</th><th>结果</th><th>说明</th></tr></thead>
<tbody>{{range .BatchResults}}<tr><td class="mono">{{.ID}}</td><td>{{if eq .Status "失败"}}<span class="badge err">{{.Status}}</span>{{else if eq .Status "已批准"}}<span class="badge ok">{{.Status}}</span>{{else}}<span class="badge">{{.Status}}</span>{{end}}</td><td class="muted">{{.Error}}</td></tr>{{end}}</tbody>
</table>
</div>
{{end}}
<div class="actions"><a class="btn ghost small" href="/candidates">返回候选列表</a></div>
{{end}}

{{define "conflicts"}}
<div class="pagehead"><div class="grow"><h1>冲突与重复</h1>
<p class="sub">这里只列出待处理的关系（本地词法扫描产生）。判定与合并仍用 <span class="mono">recalldock conflicts show/judge</span>；界面不提供自动合并。</p></div></div>
{{if .Relations}}
<div class="table-wrap">
<table><thead><tr><th>ID</th><th>关系</th><th>状态</th><th>来源</th><th>目标</th><th>时间</th></tr></thead>
<tbody>
{{range .Relations}}<tr>
  <td class="id">{{.ID}}</td>
  <td><span class="badge accent">{{.Relation}}</span></td>
  <td><span class="badge">{{.JudgmentStatus}}</span></td>
  <td><div class="cell-title">{{.SourceTitle}}</div><div class="mono faint">{{.SourceID}}</div></td>
  <td><div class="cell-title">{{.TargetTitle}}</div><div class="mono faint">{{.TargetID}}</div></td>
  <td class="muted nowrap" title="{{fmtTime .CreatedAt}}">{{ago .CreatedAt}}</td>
</tr>{{end}}
</tbody></table>
</div>
{{else}}<div class="empty"><span class="glyph">⚠</span>没有待处理的关系。可以先运行 <code>recalldock conflicts scan --dry-run</code> 看看。</div>{{end}}
{{end}}

{{define "edit"}}
<div class="pagehead"><div class="grow"><h1>编辑记忆</h1>
<p class="sub">保存会用当前快照摘要做 CAS：如果这条记忆在打开页面后被改过，保存会被拒绝，请重新打开再改。旧版本会留在历史里。</p></div></div>
<form method="post" action="/memory/{{.Obs.ID}}/update">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <input type="hidden" name="expected" value="{{.Digest}}">
  <div class="field" style="margin-bottom:14px"><label for="title">标题</label><input id="title" type="text" name="title" value="{{.Obs.Title}}"></div>
  <div class="field" style="margin-bottom:14px"><label for="abstract">摘要</label><input id="abstract" type="text" name="abstract" value="{{.Obs.Abstract}}"></div>
  <div class="field" style="margin-bottom:14px"><label for="content">正文</label><textarea id="content" name="content" style="min-height:340px">{{.Obs.Content}}</textarea></div>
  <div class="field" style="margin-bottom:16px"><label for="reason">修改理由（必填）</label><input id="reason" type="text" name="reason" required placeholder="例如：修正版本号笔误"></div>
  <div class="actions" style="margin-top:0"><button type="submit">保存新修订</button><a class="btn quiet small" href="/memory/{{.Obs.ID}}">取消</a></div>
</form>
{{end}}

{{define "similar"}}
<div class="pagehead">
  <div class="grow">
    <h1>相似记忆</h1>
    <p class="sub">只读扫描当前范围内词法相近的活跃记忆。这里不会自动合并：先生成草稿，改完保存；保存时用快照摘要做 CAS，来源随后归档到底层，仍可在「已归档」恢复。</p>
  </div>
</div>
<form method="get" action="/similar" class="filters">
  <div class="field"><label for="threshold">相似度阈值</label>
    <select id="threshold" name="threshold">
      <option value="0.50"{{if eq (printf "%.2f" .SimilarThreshold) "0.50"}} selected{{end}}>0.50 · 更宽（噪声多）</option>
      <option value="0.55"{{if eq (printf "%.2f" .SimilarThreshold) "0.55"}} selected{{end}}>0.55</option>
      <option value="0.60"{{if eq (printf "%.2f" .SimilarThreshold) "0.60"}} selected{{end}}>0.60 · 默认</option>
      <option value="0.65"{{if eq (printf "%.2f" .SimilarThreshold) "0.65"}} selected{{end}}>0.65</option>
      <option value="0.70"{{if eq (printf "%.2f" .SimilarThreshold) "0.70"}} selected{{end}}>0.70 · 更严</option>
      <option value="0.80"{{if eq (printf "%.2f" .SimilarThreshold) "0.80"}} selected{{end}}>0.80</option>
    </select>
  </div>
  <div class="field"><label>&nbsp;</label><button type="submit">重新扫描</button></div>
  <p class="muted" style="flex:1;margin:0;font-size:12.5px">范围：<strong>{{.ProjectLabel}}</strong>。扫描只读，不会改正文或归档。</p>
</form>
{{if .SimilarGroups}}
<p class="muted" style="margin:0 0 14px">发现 <strong>{{len .SimilarGroups}}</strong> 组相似记忆（阈值 {{printf "%.2f" .SimilarThreshold}}）。每组建议以正文最长的一条为目标。</p>
{{range .SimilarGroups}}
<div class="card" style="margin:0 0 14px">
  <div class="group-head">
    <h3>相似度 {{.Score}}</h3>
    <span class="badge accent">{{.MemberCount}} 条</span>
    <span class="mono muted">{{.ProjectLabel}}</span>
  </div>
  <div class="table-wrap" style="box-shadow:none">
  <table style="min-width:0">
  <thead><tr><th>#</th><th>标题</th><th>内容</th><th></th></tr></thead>
  <tbody>
  {{range .Members}}<tr>
    <td class="id">{{.ID}}</td>
    <td><div class="cell-title"><a href="/memory/{{.ID}}">{{.Title}}</a></div></td>
    <td><div class="cell-sub">{{.Preview}}</div></td>
    <td>{{if .Suggested}}<span class="badge accent">建议目标</span>{{end}}</td>
  </tr>{{end}}
  </tbody>
  </table>
  </div>
  {{if not $.ReadOnly}}
  <form method="post" action="/similar/draft" class="actions" style="margin-bottom:0">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="target" value="{{.TargetID}}">
    {{range .Members}}<input type="hidden" name="sources" value="{{.ID}}">{{end}}
    <button type="submit" class="ghost small">生成精炼草稿 →</button>
    <span class="muted" style="font-size:12.5px">目标 #{{.TargetID}}；草稿只提取来源独有的句子，不会直接写库。</span>
  </form>
  {{end}}
</div>
{{end}}
{{else}}<div class="empty"><span class="glyph">⧉</span>这个范围没有发现相似组。可以降低阈值，或先积累更多记忆；单条记忆也可以直接在详情页编辑。</div>{{end}}
{{end}}

{{define "refine"}}
<div class="pagehead">
  <div class="grow">
    <h1>精炼草稿</h1>
    <p class="sub">目标：<a href="/memory/{{.Refine.TargetID}}">#{{.Refine.TargetID}} {{.Refine.TargetTitle}}</a>{{if .Refine.Project}} · <span class="mono">{{.Refine.Project}}</span>{{end}}</p>
  </div>
</div>
<div class="notice">草稿把来源独有的句子放在「{{"【待人工精炼】"}}」下面。请把它们合并进正文并删掉整段标记，再保存；带着标记保存会被拒绝。保存成功后，下列来源会归档到底层，退出召回与复核，但仍可按 ID 查看、可在「已归档」恢复。</div>
{{range .Refine.Notes}}<div class="notice">{{.}}</div>{{end}}
<form method="post" action="/similar/apply">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <input type="hidden" name="target" value="{{.Refine.TargetID}}">
  <input type="hidden" name="expected" value="{{.Refine.Digest}}">
  {{range .Refine.Sources}}<input type="hidden" name="sources" value="{{.ID}}">{{end}}
  <div class="field" style="margin-bottom:14px"><label for="content">精炼正文</label><textarea id="content" name="content" style="min-height:380px">{{.Refine.Content}}</textarea></div>
  <div class="field" style="margin-bottom:16px"><label for="reason">修改并归档的理由（必填）</label><input id="reason" type="text" name="reason" required placeholder="例如：合并两条重复的升级踩坑记录"></div>
  <div class="actions" style="margin-top:0"><button type="submit">保存精炼并归档来源</button><a class="btn quiet small" href="/similar">取消</a></div>
</form>
{{if .Refine.Sources}}<h2>将被归档的来源<span class="after">内容仍可查看</span></h2>
<div class="table-wrap"><table>
<thead><tr><th>#</th><th>标题</th><th>内容</th></tr></thead>
<tbody>{{range .Refine.Sources}}<tr><td class="id">{{.ID}}</td><td><a href="/memory/{{.ID}}">{{.Title}}</a></td><td><div class="cell-sub">{{.Preview}}</div></td></tr>{{end}}</tbody>
</table></div>{{end}}
{{if .Refine.Skipped}}<h2>已跳过的来源</h2>
<div class="table-wrap"><table>
<thead><tr><th>#</th><th>原因</th></tr></thead>
<tbody>{{range .Refine.Skipped}}<tr><td class="id">{{.ID}}</td><td class="muted">{{.Reason}}</td></tr>{{end}}</tbody>
</table></div>{{end}}
{{end}}

{{define "refineresult"}}
<div class="pagehead"><div class="grow"><h1>部分来源未归档</h1>
<p class="sub">{{if .RefineUpdated}}精炼正文已保存。{{else}}正文没有变化。{{end}}已归档 {{.RefineAbsorbed}} 条来源；下列来源保持原状，可修正后单独重试。</p></div></div>
{{if .RefineFailures}}
<div class="table-wrap"><table>
<thead><tr><th>#</th><th>原因</th></tr></thead>
<tbody>{{range .RefineFailures}}<tr><td class="id">{{.ID}}</td><td class="muted">{{.Reason}}</td></tr>{{end}}</tbody>
</table></div>
{{end}}
<div class="actions"><a class="btn ghost small" href="/memory/{{.RefineTarget}}">查看目标记忆</a><a class="btn quiet small" href="/similar">返回相似列表</a></div>
<div class="notice" style="margin-top:16px">剩余来源可以用 <code>recalldock memory consolidate {{.RefineTarget}} --source ID... --reason "..."</code> 单独重试；置顶来源需要先取消置顶。</div>
{{end}}

{{define "archived"}}
<div class="pagehead"><div class="grow"><h1>已归档</h1>
<p class="sub">归档保留知识与历史，只从默认召回中排除；点标题进入详情可恢复。</p></div></div>
{{if .ArchivedGroups}}
{{range .ArchivedGroups}}
<div class="group">
  <div class="group-head"><h3>{{if .Name}}{{.Name}}{{else}}{{.Project}}{{end}}</h3><span class="mono faint ellip" title="{{.Project}}">{{.Project}}</span><span class="badge">{{len .Items}}</span></div>
  <div class="table-wrap">
  <table><thead><tr><th>#</th><th>标题</th><th>项目</th><th>修订</th><th>归档时间</th></tr></thead>
  <tbody>{{range .Items}}<tr><td class="id">{{.ID}}</td><td><div class="cell-title"><a href="/memory/{{.ID}}">{{.Title}}</a></div></td><td class="mono muted">{{.Project}}</td><td class="muted">{{.Revision}}</td><td class="muted nowrap" title="{{fmtTime .ArchivedAt}}">{{ago .ArchivedAt}}</td></tr>{{end}}</tbody></table>
  </div>
</div>
{{end}}
{{else}}<div class="empty"><span class="glyph">⌸</span>没有已归档的记忆。归档操作在记忆详情页。</div>{{end}}
{{end}}

{{define "projects"}}
<div class="pagehead"><div class="grow"><h1>项目</h1>
<p class="sub">项目身份、目录绑定、会话激活是三件事：这里只读展示，绑定/解绑仍用 <span class="mono">recalldock projects bind|unbind</span>。</p></div></div>
<div class="card info" style="margin-bottom:20px">
  <div class="k">当前目录</div>
  <div class="mono" style="margin:4px 0 6px;word-break:break-all">{{.CurrentDir}}</div>
  {{if .CurrentProject}}<div>解析到 <strong class="mono">{{.CurrentProject}}</strong>
    {{if .CurrentBound}}<span class="badge ok">已绑定</span>{{else}}<span class="badge warn">未绑定 / 仅配置</span>{{end}}</div>
  {{else}}<div class="muted">未解析到项目：模型会话会要求先绑定；只读命令返回空范围。</div>{{end}}
</div>
{{if .Projects}}
<div class="table-wrap">
<table><thead><tr><th>名称</th><th>身份 ID</th><th>记忆</th><th>创建时间</th><th>绑定目录</th><th>操作</th></tr></thead>
<tbody>
{{range .Projects}}
<tr>
  <td><strong>{{.Name}}</strong></td>
  <td class="mono muted">{{.ID}}</td>
  <td class="mono">{{index $.ProjectCounts .ID}}</td>
  <td class="muted nowrap">{{fmtTime .CreatedAt}}</td>
  <td>
    {{if index $.Bindings .ID}}{{range index $.Bindings .ID}}<div class="mono" style="margin-bottom:2px">{{.RootPath}} <span class="badge" title="{{.State}}">{{binding .State}}</span></div>{{end}}{{else}}<span class="muted">未绑定</span>{{end}}
  </td>
  <td><a class="btn quiet small" href="/project?value={{.ID}}&next=/projects">切换到此项目</a></td>
</tr>
{{end}}
</tbody></table>
</div>
{{else}}<div class="empty"><span class="glyph">▤</span>还没有注册项目。用 <code>recalldock projects create --name NAME</code> 创建身份，再 <code>projects bind</code> 绑定目录。</div>{{end}}
{{end}}

{{define "backups"}}
<div class="pagehead"><div class="grow"><h1>备份</h1>
<p class="sub">只读列表：这里不提供下载或恢复按钮。恢复步骤见 USER-GUIDE（停进程 → 另存当前库 → 替换 → 清 WAL → doctor 校验）。</p></div>
<div class="actions" style="margin:0"><a class="btn ghost small" href="/backups?verify=1">计算 SHA256</a><a class="btn quiet small" href="/backups">只看列表</a></div></div>
{{if .VerifyNotice}}<div class="notice">{{.VerifyNotice}}</div>{{end}}
{{if .Backups}}
<div class="table-wrap">
<table><thead><tr><th>文件</th><th>类型</th><th>时间</th><th>大小</th>{{if .VerifyChecksums}}<th>SHA256</th>{{end}}</tr></thead>
<tbody>
{{range .Backups}}<tr><td class="mono">{{.Name}}</td><td><span class="badge">{{.Kind}}</span></td><td class="muted nowrap">{{.ModTime}}</td><td class="muted nowrap">{{.Size}}</td>{{if $.VerifyChecksums}}<td class="mono faint">{{if .SHA256}}{{.SHA256}}{{else}}跳过{{end}}</td>{{end}}</tr>{{end}}
</tbody></table>
</div>
{{else}}<div class="empty"><span class="glyph">⛁</span>还没有备份文件。先运行 <code>recalldock backup --json</code> 生成一份。</div>{{end}}
{{end}}

{{define "doctor"}}
<div class="pagehead"><div class="grow"><h1>诊断</h1>
<p class="sub">只读检查本机存储、会话、同步和网络边界；任何一项都不修改数据。</p></div></div>
<div class="status-hero{{if eq .Doctor.Status "error"}} err{{else if ne .Doctor.Status "ok"}} warn{{end}}">
  <div><div class="k faint" style="font-size:11px;letter-spacing:.08em;text-transform:uppercase;font-weight:700">总体状态</div>
  <div class="big">{{if eq .Doctor.Status "ok"}}全部通过{{else if eq .Doctor.Status "error"}}存在错误{{else}}需要关注{{end}} <span class="badge {{if eq .Doctor.Status "ok"}}ok{{else if eq .Doctor.Status "error"}}err{{else}}warn{{end}}">{{.Doctor.Status}}</span></div></div>
  <div class="counts"><span>共 {{.Doctor.Summary.Total}} 项</span><span>通过 {{.Doctor.Summary.OK}}</span><span>警告 {{.Doctor.Summary.Warnings}}</span><span>错误 {{.Doctor.Summary.Errors}}</span><span>阻塞 {{.Doctor.Summary.Blocked}}</span></div>
  <div style="margin-left:auto">{{if .DoctorAll}}<a class="btn quiet small" href="/doctor">只看问题</a>{{else}}<a class="btn quiet small" href="/doctor?all=1">显示全部 {{.Doctor.Summary.Total}} 项</a>{{end}}</div>
</div>
<div class="table-wrap">
<table><thead><tr><th>检查</th><th>结果</th><th>原因</th><th>下一步</th></tr></thead>
<tbody>
{{range .Doctor.Checks}}{{if or $.DoctorShowAll (ne .Result "ok")}}
<tr>
  <td class="mono">{{.CheckID}}</td>
  <td>{{if eq .Result "ok"}}<span class="badge ok">ok</span>{{else if or (eq .Result "error") (eq .Result "blocked")}}<span class="badge err">{{.Result}}</span>{{else}}<span class="badge warn">{{.Result}}</span>{{end}}</td>
  <td class="mono" style="font-size:12px">{{.ReasonCode}}</td>
  <td class="muted">{{nextstep .SafeNextStep}}</td>
</tr>
{{end}}{{end}}
</tbody></table>
</div>
{{if and (not .DoctorAll) (not .DoctorShowAll)}}<p class="muted" style="margin-top:12px">已隐藏 {{.Doctor.Summary.OK}} 项通过的检查。</p>{{end}}
{{end}}
`
