/**
 * The proof page stylesheet: dark tokens from lib/ui-tokens/index.css, GitHub-style diffs, the
 * chapter player and the CSS-only still lightbox. The page CSP hashes the final <style> text, so
 * per-page rules are appended to this string, never written as style attributes.
 */
export const PROOF_STYLE = `:root{
--bg:#0c0e14;--bg-1:#12151c;--bg-2:#181c25;--bg-3:#1f2331;--bg-code:#0f121a;
--text:#b3bcd0;--text-h:#f3f5fa;--text-dim:#6b7387;--text-faint:#4a5061;
--border:#232838;--border-strong:#2e3548;--border-faint:#1c2030;
--accent:#4b9df8;--accent-h:#74b5fa;--accent-bg:rgba(75,157,248,.12);
--success:#5edc9b;--warning:#f4c267;--danger:#ff7a85;
--sans:-apple-system,BlinkMacSystemFont,'Inter',system-ui,'Segoe UI',Roboto,sans-serif;
--mono:'JetBrains Mono','SF Mono',ui-monospace,Menlo,Consolas,monospace;
--radius:6px;--radius-lg:10px;--ease:cubic-bezier(.22,1,.36,1);color-scheme:dark}
*{box-sizing:border-box}
html{scroll-padding-top:52px}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 var(--sans);letter-spacing:.005em;-webkit-font-smoothing:antialiased;overflow-x:hidden}
a{color:var(--accent);text-decoration:none}
a:hover{color:var(--accent-h)}
a:focus-visible,button:focus-visible,summary:focus-visible{outline:2px solid var(--accent-h);outline-offset:2px;border-radius:3px}
code{font:12.5px var(--mono);color:var(--text-h)}
h1,h2,h3,h4{color:var(--text-h);margin:0}
h2{font-size:16px;font-weight:600;display:flex;align-items:baseline;gap:8px}
h2 .n{font:400 13px var(--sans);color:var(--text-dim)}
.wrap{max-width:1080px;margin:0 auto;padding:0 32px 96px}
.dim{color:var(--text-dim)}
.warn{color:var(--warning)}
.ok{color:var(--success)}
.bad{color:var(--danger)}
.ins{color:#3fb950}.rem{color:#f85149}
.secnav{position:sticky;top:0;z-index:20;background:var(--bg);border-bottom:1px solid var(--border-faint)}
.secnav .wrap{display:flex;gap:20px;align-items:center;height:44px;padding-bottom:0;overflow-x:auto;white-space:nowrap}
.secnav a{color:var(--text-dim);font-size:13px}
.secnav a:hover{color:var(--text-h)}
.secnav .sp{flex:1}
.secnav .pin{font:12px var(--mono);color:var(--text-faint)}
.health{margin:16px 0 0;padding:10px 14px;border:1px solid rgba(244,194,103,.3);border-radius:var(--radius);color:var(--warning);font-size:13px}
header.pr{padding:28px 0 0}
.crumbs{font-size:13px;color:var(--text-dim)}
.crumbs .repo{color:var(--text);font-weight:500}
header.pr h1{font-size:22px;line-height:1.3;font-weight:600;margin:6px 0 12px;max-width:62ch;overflow-wrap:anywhere}
.meta{display:flex;flex-wrap:wrap;gap:6px 18px;font-size:13px;color:var(--text-dim)}
.meta b{color:var(--text);font-weight:500}
.meta code{color:var(--text)}
.does{margin:14px 0 0;padding-left:18px;font-size:13.5px;max-width:80ch}
.does li+li{margin-top:3px}
.does-source,.headline{font-size:12.5px;margin:6px 0 0}
.status{display:grid;grid-template-columns:repeat(4,auto) 1fr;gap:28px;align-items:end;margin:26px 0 0;padding:16px 0;border-top:1px solid var(--border-faint);border-bottom:1px solid var(--border-faint)}
.stat dt{font-size:12px;color:var(--text-dim)}
.stat dd{margin:2px 0 0;color:var(--text-h);font-size:15px;font-weight:500}
.stat dd small{color:var(--text-dim);font-size:13px;font-weight:400}
.split{justify-self:end;width:280px;max-width:100%}
.split .bar{display:flex;height:6px;border-radius:3px;overflow:hidden;background:var(--bg-3);gap:2px}
.split .bar span{display:block}
.split .key{display:flex;justify-content:space-between;gap:8px;font-size:12px;color:var(--text-dim);margin-top:6px}
.k-logic{background:var(--accent)}.k-test{background:#3a7d5e}.k-gen{background:var(--border-strong)}
section{margin-top:44px}
section>h2{margin-bottom:12px}
.reasons{list-style:none;margin:0;padding:0;counter-reset:r}
.reasons li{display:grid;grid-template-columns:28px 1fr auto;gap:12px;padding:12px 0;border-bottom:1px solid var(--border-faint);counter-increment:r}
.reasons li::before{content:counter(r);font:12px var(--mono);color:var(--text-faint);padding-top:2px}
.reasons .t{color:var(--text-h);font-weight:500;overflow-wrap:anywhere}
.reasons .d{color:var(--text-dim);font-size:13px;max-width:72ch}
.reasons .x{font-size:13px;padding-top:1px;white-space:nowrap}
.reasons li.proof .t{color:var(--danger)}
.risks{list-style:none;margin:0;padding:0;border:1px solid var(--border);border-radius:var(--radius-lg);background:var(--bg-1)}
.risks li{display:grid;grid-template-columns:76px minmax(0,1fr) 200px;gap:16px;padding:14px 18px}
.risks li+li{border-top:1px solid var(--border-faint)}
.risks .lvl{font-size:13px;font-weight:500;padding-top:1px}
.risks .lvl.high{color:var(--danger)}.risks .lvl.medium{color:var(--warning)}.risks .lvl.low{color:var(--text-dim)}.risks .lvl.deploy{color:var(--accent)}
.risks h3{font-size:14px;font-weight:600;margin-bottom:3px}
.risks p{margin:0;font-size:13.5px;max-width:74ch}
.risks .if{color:var(--text-dim);font-size:13px;margin-top:4px}
.risks ul.items{margin:4px 0 0;padding-left:18px;font-size:13px}
.risks .where{text-align:right;font:12px var(--mono);padding-top:2px;overflow-wrap:anywhere}
.risks .where span{display:block;color:var(--text-faint);font-family:var(--sans);margin-bottom:2px}
.plan li{grid-template-columns:76px minmax(0,1fr)}
.areas{border:1px solid var(--border);border-radius:var(--radius-lg);background:var(--bg-1)}
.area+.area{border-top:1px solid var(--border-faint)}
.area>summary{list-style:none;cursor:pointer;display:grid;grid-template-columns:110px minmax(0,1fr) auto;gap:16px;padding:14px 18px;align-items:baseline}
.area>summary::-webkit-details-marker{display:none}
.area>summary:hover{background:var(--bg-2)}
.area .name{color:var(--text-h);font-weight:600;overflow-wrap:anywhere}
.area .sum{font-size:13.5px}
.area .facts{display:block;margin-top:4px;font-size:12.5px;color:var(--text-dim)}
.area .facts .warn::before{content:'! ';font-weight:700}
.area .count{font-size:12.5px;color:var(--text-dim);white-space:nowrap;display:flex;gap:10px;align-items:center}
.chev{width:7px;height:7px;border-right:1.5px solid currentColor;border-bottom:1.5px solid currentColor;transform:rotate(-45deg);transition:transform .2s var(--ease)}
.area[open] .chev{transform:rotate(45deg)}
.area .body{padding:0 18px 18px 144px;min-width:0}
.area .body>p{margin:0 0 10px;font-size:13px}
.area .body figure:not(.diff){margin:12px 0 0}
.area .body img,.area .body video{max-width:100%;border-radius:var(--radius)}
.area .body pre.plain{margin:12px 0 0;background:var(--bg-code);border:1px solid #30363d;border-radius:var(--radius);padding:10px 12px;font:12px/1.5 var(--mono);color:#e6edf3;overflow:auto}
.diff{margin:12px 0 0;border:1px solid #30363d;border-radius:var(--radius);overflow:hidden;background:var(--bg-code)}
.diff:first-child{margin-top:0}
.diff figcaption{display:flex;align-items:center;gap:12px;padding:7px 12px;background:#151a22;border-bottom:1px solid #30363d;font-size:12.5px}
.diff .path{font:12.5px var(--mono);color:#e6edf3;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.diff .stat{font:500 12px var(--mono);display:flex;align-items:center;gap:6px}
.diff .stat .plus{color:#3fb950}.diff .stat .minus{color:#f85149}
.diff .scroll{overflow-x:auto}
.diff table{border-collapse:collapse;width:100%;font:12.5px/20px var(--mono);color:#e6edf3}
.diff td{padding:0;vertical-align:top}
.diff td.n{width:1%;min-width:42px;padding:0 8px;text-align:right;color:#6e7681;user-select:none;font-size:12px}
.diff td.t{padding:0 16px 0 4px;white-space:pre}
.diff .sign{display:inline-block;width:16px;color:#6e7681;user-select:none}
.diff tr.add td.t{background:rgba(46,160,67,.15)}
.diff tr.add td.n{background:rgba(46,160,67,.25);color:#adbac7}
.diff tr.add .sign{color:#3fb950}
.diff tr.del td.t{background:rgba(248,81,73,.15)}
.diff tr.del td.n{background:rgba(248,81,73,.25);color:#adbac7}
.diff tr.del .sign{color:#f85149}
.diff tr.hunk td{background:rgba(56,139,253,.1);color:#8b949e;padding-top:3px;padding-bottom:3px}
.diff tr.hunk td.t{padding-left:12px}
.diff tr.pinned td.t,.diff tr.pinned td.n{background:rgba(244,194,103,.16)}
.diff tr.pinned td.n{color:var(--warning)}
.diff .k{color:#ff7b72}.diff .s{color:#a5d6ff}.diff .c{color:#8b949e}.diff .f{color:#d2a8ff}
.diff .more{padding:7px 12px;border-top:1px solid #30363d;font-size:12.5px;color:var(--text-dim)}
.pinnote{padding:9px 14px 10px 62px;background:rgba(244,194,103,.06);border-top:1px solid rgba(244,194,103,.22);font-size:13px;color:var(--text);position:relative}
.pinnote .pinicon{position:absolute;left:24px;top:9px;color:var(--warning);font-weight:700}
.evidence{display:grid;grid-template-columns:minmax(0,1fr) 280px;border:1px solid var(--border);border-radius:var(--radius-lg);overflow:hidden;background:var(--bg-1)}
.screen{position:relative;height:560px;background:#07080b}
.screen video{position:absolute;inset:0;width:100%;height:100%;object-fit:contain;background:#07080b}
.chapters{border-left:1px solid var(--border);display:flex;flex-direction:column;min-width:0}
.chapters h3{font-size:13px;font-weight:600;padding:12px 14px;border-bottom:1px solid var(--border-faint);display:flex;justify-content:space-between;gap:8px}
.chapters h3 span{color:var(--text-dim);font-weight:400}
.chapters ol{list-style:none;margin:0;padding:4px 0;overflow:auto}
.chapters .grp{font-size:12px;color:var(--text-faint);padding:10px 14px 2px}
.chapters button{all:unset;cursor:pointer;display:grid;grid-template-columns:40px 1fr;gap:8px;padding:8px 14px;width:100%;box-sizing:border-box;font-size:13px;color:var(--text)}
.chapters button:hover{background:var(--bg-2)}
.chapters button.cur{background:var(--accent-bg);color:var(--text-h)}
.chapters .ts{font:12px var(--mono);color:var(--accent);padding-top:1px}
.nowplaying{font-size:12.5px;color:var(--text-dim);margin:8px 0 0}
.evnote{font-size:12.5px;color:var(--text-dim);margin:10px 0 0}
.sub{font-size:13px;color:var(--text-dim);margin:18px 0 8px;font-weight:600}
.pair{display:flex;flex-wrap:wrap;gap:16px;margin-bottom:12px}
.pair figure{margin:0;width:220px;max-width:calc(50% - 8px)}
.pair img{width:100%;max-height:360px;object-fit:contain;background:#f1f2f4;border:1px solid var(--border);border-radius:var(--radius)}
.pair figcaption{font-size:12.5px;color:var(--text-dim);padding-top:4px}
.thumbs{display:flex;flex-wrap:wrap;gap:12px;margin-top:14px}
.thumb{display:block;width:110px;color:var(--text);font-size:12.5px}
.thumb img{display:block;width:100%;height:190px;object-fit:contain;background:#f1f2f4;border:1px solid var(--border);border-radius:var(--radius);transition:border-color .15s var(--ease)}
.thumb:hover img{border-color:var(--accent)}
.thumb span{display:block;padding-top:6px;color:var(--text-dim);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.thumb:hover span{color:var(--text-h)}
.lightbox{position:fixed;inset:0;z-index:100;display:none;place-items:center}
.lightbox:target{display:grid}
.lb-bg{position:absolute;inset:0;background:rgba(7,8,11,.86)}
.lightbox figure{position:relative;margin:0;max-width:86vw;display:flex;flex-direction:column}
.lightbox img{max-width:86vw;max-height:80vh;object-fit:contain;border-radius:var(--radius);background:#f4f5f8}
.lightbox figcaption{color:var(--text);font-size:13px;padding:10px 0;display:flex;gap:8px}
.lightbox figcaption b{color:var(--text-h);font-weight:500}
.lb-x{margin-left:auto;color:var(--text)}
.lb-prev,.lb-next{position:absolute;top:50%;transform:translateY(-50%);font-size:36px;color:var(--text-h);padding:10px 18px;z-index:2}
.lb-prev{left:6px}.lb-next{right:6px}
.tx{border:1px solid var(--border);border-radius:var(--radius-lg);background:var(--bg-1);padding:14px 18px;margin-top:12px}
.tx header{display:flex;flex-wrap:wrap;gap:10px;align-items:baseline;font-size:13.5px}
.tx header b{color:var(--text-h);font-weight:600;flex:1}
.tx p{margin:4px 0 10px;font-size:13px}
.txcols{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:12px}
.tx h4{font-size:12px;color:var(--text-dim);margin:6px 0 4px;font-weight:500}
.tx pre{margin:0;background:var(--bg-code);border:1px solid #30363d;border-radius:var(--radius);padding:10px 12px;font:12px/1.5 var(--mono);color:#e6edf3;overflow:auto;max-height:320px}
.tx pre.rows{max-height:none}
.checks{display:grid;grid-template-columns:1fr 1fr;gap:40px}
.checks h3{font-size:13px;font-weight:600;color:var(--text-dim);margin-bottom:4px}
.checks ul{list-style:none;margin:0;padding:0}
.checks li{display:flex;gap:10px;padding:9px 0;font-size:13.5px;border-bottom:1px solid var(--border-faint)}
.box{flex:none;width:15px;height:15px;border:1.5px solid var(--border-strong);border-radius:4px;margin-top:3px}
.box.done{background:var(--success);border-color:var(--success)}
.notes{list-style:none;margin:0;padding:0}
.notes li{display:grid;grid-template-columns:28px minmax(0,220px) minmax(0,1fr);gap:12px;padding:9px 0;border-bottom:1px solid var(--border-faint);font-size:13.5px;align-items:baseline}
.notes .sev{font:12px var(--mono);color:var(--text-faint)}
.notes .loc{font-size:12px;color:var(--text-dim);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
details.more{margin-top:10px;border-top:1px solid var(--border-faint)}
details.more>summary{cursor:pointer;padding:12px 0;color:var(--text-h);font-weight:600;list-style:none;display:flex;gap:10px;align-items:center}
details.more>summary::-webkit-details-marker{display:none}
details.more>summary .dim{font-weight:400}
.ftable{width:100%;border-collapse:collapse;font-size:12.5px;table-layout:fixed}
.ftable td{padding:4px 0;border-top:1px solid var(--border-faint)}
.ftable td+td{padding-left:14px}
.ftable .fp{font-family:var(--mono);color:var(--text);font-size:12px;overflow-wrap:anywhere}
.ftable tr.gen td,.ftable tr.gen .fp{color:var(--text-faint)}
.ftable .num{font-family:var(--mono);text-align:right;width:64px}
.timeline{list-style:none;margin:0;padding:0 0 8px}
.timeline li{display:flex;gap:14px;padding:4px 0;font-size:13px}
.timeline time{font:12px var(--mono);color:var(--text-dim);flex:none;width:92px}
.qa{margin-top:44px;border:1px solid var(--border);border-radius:var(--radius-lg);padding:14px 18px;font-size:13px}
.qa h2{font-size:13px;margin-bottom:6px}
.qa h3{font-size:13px;margin:10px 0 6px}
.qa ul{margin:0;padding-left:18px}
.qa.flagged{border-color:rgba(255,122,133,.4)}
footer{margin-top:48px;color:var(--text-faint);font-size:12px;display:flex;gap:18px;flex-wrap:wrap}
footer code{color:var(--text-faint);font-size:12px}
@media (max-width:760px){
.wrap{padding:0 16px 64px}
.status{grid-template-columns:repeat(2,minmax(0,1fr));gap:16px}
.split{grid-column:1/-1;justify-self:stretch;width:auto}
.reasons li{grid-template-columns:22px minmax(0,1fr)}
.reasons .x{grid-column:2}
.risks li,.plan li{grid-template-columns:minmax(0,1fr);gap:4px}
.risks .where{text-align:left}
.area>summary{grid-template-columns:minmax(0,1fr) auto}
.area .sum{grid-column:1/-1;grid-row:2}
.area .body{padding:0 12px 14px}
.evidence{grid-template-columns:minmax(0,1fr)}
.screen{height:420px}
.chapters{border-left:0;border-top:1px solid var(--border)}
.checks,.txcols{grid-template-columns:minmax(0,1fr);gap:16px}
.notes li{grid-template-columns:28px minmax(0,1fr)}
.notes li>span:last-child{grid-column:1/-1}
}
@media (prefers-reduced-motion:reduce){*{transition:none!important}}`
