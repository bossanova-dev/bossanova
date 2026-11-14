/**
 * The proof page stylesheet: the design tokens (dark by default, light by system preference or
 * `data-theme`), the confidence banner, ruled item lists, diffs in the theme's add and remove
 * colours, describer blocks (schema, tables, routes, client calls), the Files grid, the chapter
 * player and the lightbox. The page CSP hashes the final <style> text, so per-page rules are
 * appended to this string, never written as style attributes. Radii, type sizes, spacing and pills
 * follow the web app's own stylesheet.
 */
import { PROOF_TOKENS } from './verify-proof-tokens.mjs'

export const PROOF_STYLE = `${PROOF_TOKENS}
*{box-sizing:border-box}
html{scroll-padding-top:56px}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 var(--sans);letter-spacing:.005em;-webkit-font-smoothing:antialiased;overflow-x:hidden;overflow-wrap:anywhere}
body.lb-open{overflow:hidden}
a{color:var(--accent);text-decoration:none}
a:hover{color:var(--accent-h)}
a:focus-visible,button:focus-visible,input:focus-visible,summary:focus-visible{outline:2px solid var(--accent-h);outline-offset:2px;border-radius:var(--radius-sm)}
code{font:.9em var(--mono);color:var(--text-h);background:var(--bg-code);border:1px solid var(--border-faint);border-radius:var(--radius);padding:0 6px}
.loc,.sha,.debug code,.foot code{background:none;border:0;padding:0}
a.tk{color:inherit;text-decoration:underline;text-decoration-color:var(--border-strong);text-underline-offset:2px}
a.tk:hover{color:var(--accent-h)}
h1,h2,h3,h4{color:var(--text-h);margin:0}
.wrap{max-width:1080px;margin:0 auto;padding:24px 24px 96px}
.dim{color:var(--text-dim)}
.pill{display:inline-flex;align-items:center;font-size:11px;font-weight:600;letter-spacing:.08em;padding:4px 10px;margin-block:-3px;border:1px solid transparent;border-radius:var(--radius-pill);white-space:nowrap;line-height:1.4;flex-shrink:0;color:color-mix(in oklab,var(--text) 70%,var(--text-dim));background:var(--bg-elev-3)}
.pill.success{color:var(--success);background:var(--success-bg)}
.pill.warning{color:var(--warning);background:var(--warning-bg)}
.pill.danger{color:var(--danger);background:var(--danger-bg)}
.pill.info{color:var(--info);background:var(--accent-bg)}
.diffstat{font-family:var(--mono);font-weight:500;white-space:nowrap;font-variant-numeric:tabular-nums}
.diffstat .a{color:var(--success)}.diffstat .d{color:var(--danger)}
.blocks{display:inline-flex;gap:2px;margin-left:8px;vertical-align:middle}
.blocks i{width:8px;height:8px;border-radius:var(--radius-sm);background:var(--border-strong);display:inline-block}
.blocks i.a{background:var(--success)}.blocks i.d{background:var(--danger)}
.bnotice{display:flex;gap:10px;align-items:flex-start;padding:10px 16px;margin-bottom:16px;border-radius:var(--radius);font-size:13.5px;border:1px solid}
.bnotice .bi{width:16px;height:16px;margin-top:2px;flex-shrink:0}
.bnotice b{display:block;color:var(--text-h);font-weight:600}
.bnotice span{color:var(--text)}
.bnotice a{color:inherit;text-decoration:underline;text-underline-offset:2px}
.bnotice.stacked{background:var(--warning-bg);border-color:color-mix(in oklab,var(--warning) 35%,transparent);color:var(--warning)}
.bnotice.branch{background:var(--accent-bg);border-color:var(--accent-border);color:var(--info)}
header.pr h1{font-size:16px;line-height:1.35;font-weight:600;letter-spacing:-.012em;max-width:72ch}
header.pr .meta{display:flex;flex-wrap:wrap;gap:4px 16px;align-items:center;font-size:13px;color:var(--text-dim);margin-top:4px}
header.pr .meta a{color:var(--text-dim)}header.pr .meta a:hover{color:var(--accent-h)}
header.pr a.prlink,header.pr .meta a.prlink:hover,header.pr a.prlink:hover{color:inherit}header.pr a.prlink:hover{text-decoration:underline;text-underline-offset:2px}
header.pr .sha{font:12px var(--mono);color:var(--text-dim)}
.banner{--lvl:var(--text-dim);--lvl-bg:var(--bg-elev-1);--lbl-w:104px;margin-top:24px;padding:16px 24px 8px;border-radius:var(--radius);background:var(--lvl-bg);border:1px solid color-mix(in oklab,var(--lvl) 28%,var(--border))}
.banner.lv-success{--lvl:var(--success);--lvl-bg:var(--success-bg)}.banner.lv-info{--lvl:var(--info);--lvl-bg:var(--accent-bg)}.banner.lv-warning{--lvl:var(--warning);--lvl-bg:var(--warning-bg)}.banner.lv-danger{--lvl:var(--danger);--lvl-bg:var(--danger-bg)}
.banner .lv{display:flex;align-items:baseline;gap:8px;flex-wrap:wrap}
.banner .lv b{font-size:24px;line-height:1.25;font-weight:600;letter-spacing:-.012em;color:var(--lvl)}
.banner .lv span{font-size:13px;color:var(--text-dim)}
.banner .reason{font-size:15px;color:var(--text-h);margin:8px 0 0;max-width:68ch}
.banner>:last-child{margin-bottom:8px}
.glist{margin:12px 0 0;padding:0;list-style:none;font-size:13.5px}
.glist li{display:grid;grid-template-columns:var(--lbl-w) minmax(0,1fr);gap:12px;padding:2px 0}
.glist .k{font-weight:600;color:var(--text-h)}
.gaps .k{color:var(--warning)}
.rsum a{display:inline-flex;align-items:baseline;gap:8px;color:var(--text-h)}
.rsum a:hover{color:var(--accent-h)}
.rsum .dot{margin-left:-15px}
.dot{width:7px;height:7px;border-radius:50%;background:var(--text-dim);flex-shrink:0;transform:translateY(-1px)}
.dot.success{background:var(--success)}.dot.warning{background:var(--warning)}.dot.danger{background:var(--danger)}.dot.info{background:var(--info)}
.sig{margin:16px 0 0;border-top:1px solid color-mix(in oklab,var(--lvl) 22%,var(--border))}
.sig .r{display:grid;grid-template-columns:var(--lbl-w) minmax(0,1fr) 96px;gap:12px;align-items:baseline;padding:6px 0;border-bottom:1px solid color-mix(in oklab,var(--lvl) 10%,var(--border-faint));color:var(--text);font-size:13px}
.sig .r:last-child{border-bottom:0}
.sig a.r:hover{background:color-mix(in oklab,var(--lvl) 8%,var(--bg-elev-1))}
.sig .k{font-weight:600;color:var(--text-h)}
.sig .v{display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.sig .st{justify-self:end}
.secnav{position:sticky;top:0;z-index:10;background:var(--bg);border-bottom:1px solid var(--border);margin-top:24px;padding:6px 0;display:flex;gap:2px;overflow-x:auto;scrollbar-width:none}
.secnav a{font-size:13px;color:var(--text-dim);padding:4px 10px;border-radius:var(--radius);white-space:nowrap}
.secnav a:hover{background:var(--bg-elev-2);color:var(--text-h)}
.secnav .n{font-family:var(--mono);font-size:11px;margin-left:6px;color:var(--text-faint)}
.secnav a.hot .n{color:var(--warning)}
section.sec{margin-top:32px}
section.sec>h2{font-size:15px;font-weight:600;margin:0 0 8px;display:flex;gap:8px;align-items:baseline;flex-wrap:wrap}
section.sec>h2 .n{font:400 13px var(--sans);color:var(--text-dim)}
section.sec>h2 .pill{align-self:center}
.items{border-top:1px solid var(--border)}
.item{padding:14px 0;border-bottom:1px solid var(--border-faint)}
.item .hd{display:flex;gap:8px;align-items:flex-start}
.item .hd>.pill{margin-top:-2px}
.item .q{font-size:14px;font-weight:600;color:var(--text-h);line-height:1.45;flex:1;min-width:0}
.item .p{margin:4px 0 0;max-width:72ch}
.loc{font-size:12px;font-family:var(--mono);color:var(--text-dim);white-space:nowrap;flex-shrink:0;padding-top:2px}
a.lk .loc{color:inherit}
.guard{display:grid;grid-template-columns:auto minmax(0,1fr);gap:8px;margin-top:6px;font-size:13px;color:var(--text-dim)}
.guard b{color:var(--text);font-weight:600}
.facts{margin:6px 0 0;padding-left:18px;font-size:13.5px}
.facts li{margin:2px 0}
.facts li.flag{color:var(--warning)}
.lead{margin:0 0 8px;color:var(--text-h);max-width:72ch}
.asks{list-style:none;margin:0 0 8px;padding:0;border-top:1px solid var(--border)}
.asks li{padding:8px 0;border-bottom:1px solid var(--border-faint)}
.asks b{display:block;color:var(--text-h);font-weight:600}
.check{width:16px;height:16px;margin:2px 0 0;accent-color:var(--success);flex-shrink:0;cursor:pointer}
.decided{font-size:11px;font-weight:600;letter-spacing:.08em;color:var(--success);flex-shrink:0;display:none}
.item.done{padding:6px 0}
.item.done .hd{cursor:pointer;align-items:center}
.item.done .q{color:var(--text-dim);font-weight:500;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.item.done .decided{display:inline}
.item.done .hd::after{content:"";width:6px;height:6px;border-right:1.5px solid var(--text-dim);border-bottom:1.5px solid var(--text-dim);transform:rotate(-45deg);transition:transform .15s cubic-bezier(.22,1,.36,1);flex-shrink:0;margin:0 4px}
.item.done.open{padding:14px 0}
.item.done.open .q{white-space:normal}
.item.done.open .hd::after{transform:rotate(45deg)}
.item.done:not(.open)>:not(.hd){display:none}
.pills{display:inline-flex;gap:6px;flex-shrink:0;margin-top:1px}
.links{display:inline-flex;gap:4px;flex-shrink:0;padding-top:1px}
.lk{font:11px/18px var(--mono);color:var(--text-dim);border:1px solid var(--border);border-radius:var(--radius-sm);padding:0 5px}
.lk:hover{color:var(--accent-h);border-color:var(--accent-border)}
.box{border:1px solid var(--border);border-radius:var(--radius);overflow:hidden}
.tbl{width:100%;border-collapse:collapse;font-size:13px}
.tbl td{padding:6px 12px;border-bottom:1px solid var(--border-faint);vertical-align:top}
.tbl tr:last-child td{border-bottom:0}
table.ac td.id{width:56px;max-width:56px;font:12px var(--mono);color:var(--text-dim);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
table.ac td.st{width:1%;text-align:right}
.ev{display:flex;flex-wrap:wrap;gap:4px;margin-top:4px}
.evc{font:12px var(--mono);color:var(--text-dim);border:1px solid var(--border);border-radius:var(--radius-sm);padding:0 6px;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.evc code{background:none;border:0;padding:0;font-size:inherit;color:inherit}
a.evc:hover{color:var(--accent-h);border-color:var(--accent-border)}
.todos{border-top:1px solid var(--border)}
.todo{display:flex;gap:8px;align-items:flex-start;padding:6px 0;border-bottom:1px solid var(--border-faint);font-size:13.5px;cursor:pointer}
.todo.done span{color:var(--text-dim);text-decoration:line-through;text-decoration-color:var(--text-faint)}
.sp+.sp{margin-top:24px}
.sp>h3.sub:first-child{margin-top:0}
h3.sub .h3n{font-weight:400;letter-spacing:0;text-transform:none;font-size:12px;margin-left:6px}
.cl{--lbl-w:104px;display:grid;grid-template-columns:0 auto auto minmax(0,1fr) auto auto;border-top:1px solid var(--border);font-size:13.5px}
.cl.grp{grid-template-columns:var(--lbl-w) auto auto minmax(0,1fr) auto auto}
.si{grid-column:1/-1;display:grid;grid-template-columns:subgrid;align-items:start;padding:6px 0;border-bottom:1px solid var(--border-faint)}
.si.rich{padding:12px 0}
.si.first{border-top:1px solid var(--border)}
.si:target{background:var(--accent-bg)}
.si>.g{grid-column:1;font-size:13px;font-weight:600;color:var(--text-h);line-height:1.5;padding-right:12px}
.si>.check{grid-column:2;margin:3px 12px 0 0}
.si>.no{grid-column:3;font:12.5px/1.6 var(--mono);color:var(--text-dim);text-align:right;min-width:18px;margin-right:12px;font-variant-numeric:tabular-nums}
.si>.hd{grid-column:4;display:flex;flex-direction:column;gap:3px;min-width:0}
.si>.links{grid-column:5;margin-left:12px;padding-top:1px}
.si>.st{grid-column:6;justify-self:end;margin-left:12px}
.si .cx{font-size:12.5px;color:var(--text-dim)}
.si.rich .it{color:var(--text-h);font-weight:600}
.si .ex{display:grid;grid-template-columns:auto minmax(0,1fr);gap:8px}
.si .ex b{color:var(--success);font-weight:600}
.si.done .it{color:var(--text-dim);text-decoration:line-through;text-decoration-color:var(--text-faint)}
.si.done .cx,.si.done .ex,.si.done .ex b{color:var(--text-dim)}
.si.rich.done{padding:6px 0}
.si.rich.done.open{padding:12px 0}
.si.rich.done .hd{cursor:pointer}
.si.rich.done:not(.open) .cx,.si.rich.done:not(.open) .ex{display:none}
.si.rich.done:not(.open) .it{font-weight:500;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.stbl thead th{font-size:11px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;color:var(--text-dim);text-align:left;padding:6px 12px;border-bottom:1px solid var(--border);background:var(--bg-elev-1)}
.stbl th.pc,.stbl td.pc{text-align:right}
.stbl .mono{display:block;font:12px var(--mono);color:var(--text-h)}
.sn{font-size:12.5px;line-height:1.45;color:var(--text-dim);margin-top:2px}
p.sn{margin:6px 0 0}
.tblwrap{overflow-x:auto}
ul.kv{--lbl-w:104px;list-style:none;margin:0;padding:0;border-top:1px solid var(--border);font-size:13px}
ul.kv li{display:grid;grid-template-columns:var(--lbl-w) minmax(0,1fr);gap:12px;padding:6px 0;border-bottom:1px solid var(--border-faint)}
ul.kv .k{color:var(--text-h);font-weight:600}
ul.kv .k.success{color:var(--success)}ul.kv .k.warning{color:var(--warning)}ul.kv .k.danger{color:var(--danger)}ul.kv .k.info{color:var(--info)}ul.kv .k.muted{color:var(--text-dim)}
figure{margin:0}
.code{margin:12px 0 0;border:1px solid var(--border);border-radius:var(--radius);overflow:hidden;background:var(--bg-code)}
.code figcaption{display:flex;align-items:center;gap:12px;padding:6px 12px;border-bottom:1px solid var(--border);font-size:12px}
.code .path{font:12px var(--mono);color:var(--text-dim);flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.code .scroll{overflow-x:auto}
.code table{border-collapse:collapse;width:100%;font:12px/1.6 var(--mono);color:var(--text-h)}
.code td{padding:0;vertical-align:top}
.code td.n{width:1%;padding:0 8px;text-align:right;color:var(--text-faint);user-select:none;white-space:nowrap;font-variant-numeric:tabular-nums}
.code td.n+td.n{border-right:1px solid var(--border)}
.code td.m{width:18px;padding-left:6px;color:var(--text-faint);user-select:none;white-space:nowrap}
.code td.t{padding:0 16px 0 2px;white-space:pre;tab-size:4}
.code tr.add{background:var(--success-bg)}.code tr.add td.m{color:var(--success)}
.code tr.del{background:var(--danger-bg)}.code tr.del td.m{color:var(--danger)}
.code tr.hunk td{color:var(--text-dim);background:var(--accent-bg);padding-top:2px;padding-bottom:2px}
.code tr.pinned td{background:var(--warning-bg)}
.code tr.pinned td.n{color:var(--warning);font-weight:700}
.code .cut{color:var(--warning);font-family:var(--sans);font-size:12px;margin-left:6px;user-select:none}
.code .k{color:var(--info)}.code .s{color:var(--warning)}.code .c{color:var(--text-dim)}.code .f{color:var(--accent-h)}
.code .more{padding:6px 12px;border-top:1px solid var(--border);font-size:12.5px;color:var(--text-dim)}
.pinnote{padding:8px 12px;border-top:1px solid var(--border);font-size:13px;color:var(--warning)}
h3.sub{font-size:11px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;color:var(--text-dim);margin:16px 0 6px}
pre{margin:0;font:12px/1.55 var(--mono);color:var(--text);white-space:pre-wrap;overflow-wrap:anywhere}
pre.plain{background:var(--bg-code);border:1px solid var(--border);border-radius:var(--radius);padding:8px 12px}
.player{display:grid;grid-template-columns:minmax(0,1fr) 240px;gap:16px;margin-top:12px}
.screen video{display:block;width:100%;max-height:560px;background:var(--bg-elev-2);border-radius:var(--radius)}
.chapters{max-height:560px;overflow-y:auto}
.chapters h3{font-size:12px;font-weight:600;color:var(--text-dim);margin:0 0 6px;display:flex;justify-content:space-between}
.chapters ol{list-style:none;margin:0;padding:0}
.chapters .grp{font-size:12px;font-weight:600;color:var(--text-h);padding:8px 0 4px}
.chapters button{display:grid;grid-template-columns:44px minmax(0,1fr);gap:6px;width:100%;text-align:left;background:none;border:0;border-radius:var(--radius-sm);color:var(--text);font:13px/1.4 var(--sans);padding:4px 6px;cursor:pointer}
.chapters button:hover{background:var(--bg-elev-2)}
.chapters button.cur{background:var(--accent-bg);color:var(--accent-h)}
.chapters .ts{font-family:var(--mono);font-size:12px;color:var(--text-dim);font-variant-numeric:tabular-nums}
.nowplaying{font-size:13px;color:var(--text-dim);margin:6px 0 0}
.thumbs{display:grid;grid-template-columns:repeat(auto-fill,minmax(160px,1fr));gap:12px;margin-top:12px}
.pair{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:12px}
.shot{display:block;color:var(--text);font-size:12.5px}
.shot img{display:block;width:100%;height:auto;max-height:420px;object-fit:contain;object-position:top;border:1px solid var(--border);border-radius:var(--radius);background:var(--bg-elev-2);cursor:zoom-in}
.pair .shot img{height:320px;max-height:none}
.shot:hover img{border-color:var(--border-strong)}
.shot span{display:block;margin-top:4px}
.nopage{display:flex;align-items:center;justify-content:center;min-height:120px;border:1px dashed var(--border);border-radius:var(--radius);color:var(--text-dim);font-size:12.5px}
.media{margin-top:12px}.media video{display:block;max-width:100%;border-radius:var(--radius)}
.media figcaption{font-size:12.5px;color:var(--text-dim);margin-top:4px}
.doc{margin-top:12px}
.doc header{display:flex;flex-wrap:wrap;gap:4px 12px;align-items:baseline;font-size:13px}
.doc header b{color:var(--text-h)}
.framed-note{display:none}
.proof-framed .doc header a{display:none}.proof-framed .framed-note{display:inline}
.tx{margin-top:12px;border-top:1px solid var(--border);padding-top:12px}
.tx header{display:flex;gap:8px;align-items:baseline;font-size:13px}
.tx header b{color:var(--text-h);flex:1}
.txcols{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:8px}
.tx h4{font-size:11px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;color:var(--text-dim);margin:0 0 4px}
.tx pre{background:var(--bg-code);border:1px solid var(--border);border-radius:var(--radius);padding:8px;max-height:320px;overflow:auto}
.schema{margin-top:12px;border:1px solid var(--border);border-radius:var(--radius);background:var(--bg-code);font:12.5px/1.65 var(--mono);overflow-x:auto}
.sb{padding:8px 16px 10px;border-bottom:1px solid var(--border-faint)}
.sb:last-child{border-bottom:0}
.sbh{display:flex;align-items:baseline;gap:8px;color:var(--text-dim)}
.sbh .tn{color:var(--text-h);font-weight:600}
.sbh .new{color:var(--success);font:600 11px var(--sans)}
.sbh .gone{color:var(--danger);font:600 11px var(--sans)}
.sbh .dim{font:12px var(--sans)}
.sbh .pill{margin-left:auto;align-self:center;font-family:var(--sans)}
.fields{display:grid;grid-template-columns:fit-content(60%) minmax(0,1fr);column-gap:24px;padding:2px 0 0 16px}
.fr{display:contents}
.fnc,.ftc{min-width:0}
.fn{color:var(--text-h)}.pu,.dr{color:var(--text-faint)}.ty{color:var(--info)}.nn{color:var(--warning)}
.args{display:block;padding-left:16px}.args>span{display:block}
.fr.ad .fn,.enumv.ad{color:var(--success)}
.fr.rm>span,.enumv.rm,.sb.rm .tn,.sb.rm .fields{text-decoration:line-through;text-decoration-color:var(--danger)}
.fr.rm .fn{color:var(--danger)}
.was{color:var(--text-faint);text-decoration-color:var(--danger)}
.enumvs{display:flex;flex-wrap:wrap;gap:2px 14px;padding:2px 0 0 16px;color:var(--text-h)}
.mob{display:flex;gap:8px;margin-top:6px;font:13px/1.5 var(--sans);color:var(--warning)}
.mob b{font-weight:600;flex-shrink:0}
.calls{border-top:1px solid var(--border)}
.cc{padding:10px 0;border-bottom:1px solid var(--border-faint)}
.cch{display:flex;gap:12px;align-items:baseline}
.cch .tn{font:600 12.5px var(--mono);color:var(--text-h);flex:1;min-width:0}
pre.gql{margin:6px 0 0;padding:8px 12px;background:var(--bg-code);border:1px solid var(--border);border-radius:var(--radius);white-space:pre;overflow-x:auto;color:var(--text-h)}
.gql .kw{color:var(--text-dim)}.gql .ty{color:var(--info)}.gql .nn{color:var(--warning)}
.gql .al{color:var(--success)}.gql .dl{color:var(--danger)}
.cc .note{margin:6px 0 0;font-size:13px;color:var(--text-dim);max-width:72ch}
.routes{border-top:1px solid var(--border)}
.rt{display:grid;grid-template-columns:64px minmax(0,1fr) auto;gap:12px;align-items:baseline;padding:6px 0;border-bottom:1px solid var(--border-faint);font-size:13px}
.rt .mt{font:600 12px var(--mono);color:var(--text-dim)}
.rt .ch{font-size:12px;color:var(--warning)}
.rt .dim{display:block;font-size:12.5px}
.rt.rm code{text-decoration:line-through;text-decoration-color:var(--danger)}
.tbox{margin-top:12px;border:1px solid var(--border);border-radius:var(--radius);overflow:hidden}
.tbh{display:flex;align-items:baseline;gap:8px;padding:6px 12px;border-bottom:1px solid var(--border);font-size:13px}
.tbh .nm{font:600 12.5px var(--mono);color:var(--text-h)}
.tbh .pill{margin-left:auto;align-self:center}
table.cols{width:100%;table-layout:fixed;border-collapse:collapse;font-size:13px}
table.cols col.cn{width:34%}table.cols col.ct{width:26%}
table.cols td{padding:6px 12px;border-bottom:1px solid var(--border-faint);vertical-align:top;overflow-wrap:anywhere}
table.cols tr:last-child td{border-bottom:0}
table.cols td.cn{font:12.5px var(--mono);color:var(--text-h)}
table.cols td.ct{font:12px var(--mono);color:var(--text-dim)}
table.cols td.cx{color:var(--text-dim)}
table.cols td.cx .pill{margin-right:6px}
table.cols tr.rm td.cn{text-decoration:line-through;text-decoration-color:var(--danger);color:var(--danger)}
table.cols tr.also td{font-size:12px;color:var(--text-dim)}
table.cols tr.also span{font-family:var(--mono)}
.pii{margin-left:6px;font:600 11px var(--sans);letter-spacing:.08em;color:var(--danger);background:var(--danger-bg);padding:1px 6px;border-radius:var(--radius-sm)}
.btitle{margin:12px 0 0;font-size:13px;color:var(--text-h)}
.btitle+.code{margin-top:6px}
.code.excerpt td.n{border-right:1px solid var(--border)}
.code.excerpt td.t{padding-left:12px}
.files{border-top:1px solid var(--border)}
.frow{display:grid;grid-template-columns:minmax(0,1fr) 44px 64px 64px 132px;gap:12px;align-items:center;padding:6px 0;border-bottom:1px solid var(--border-faint);font-size:13px}
.frow .c2,.frow .c3,.frow .c4{text-align:right;font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:12.5px;white-space:nowrap}
.frow .c2{color:var(--text-dim)}.frow .c3{color:var(--success)}.frow .c4{color:var(--danger)}
.frow .z{color:var(--text-faint)}
.bar{display:flex;height:8px;border-radius:var(--radius-sm);overflow:hidden}
.bar i{display:block;height:100%}.bar i.a{background:var(--success)}.bar i.d{background:var(--danger)}
details.fg>summary{list-style:none;cursor:pointer}
details.fg>summary::-webkit-details-marker{display:none}
details.fg>summary .nm{display:flex;align-items:center;gap:8px;color:var(--text-h);font-weight:600;min-width:0}
details.fg>summary .nm::before{content:"";width:6px;height:6px;border-right:1.5px solid var(--text-dim);border-bottom:1.5px solid var(--text-dim);transform:rotate(-45deg);transition:transform .15s cubic-bezier(.22,1,.36,1);flex-shrink:0;margin:0 2px}
details.fg[open]>summary .nm::before{transform:rotate(45deg)}
details.fg>summary:hover{background:var(--bg-elev-1)}
.frow.file .nm{padding-left:18px;font:12px var(--mono);color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;direction:rtl;text-align:left}
.frow.file a.nm:hover{color:var(--text-h);text-decoration:underline;text-underline-offset:2px}
.frow.head{font-size:11px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;color:var(--text-dim);border-bottom:1px solid var(--border);padding:4px 0}
.frow.head>*{text-align:right}.frow.head>:first-child,.frow.head>:last-child{text-align:left}
.debug{display:none;font-size:13px}
.proof-debug .debug{display:block}
.debug h3{font-size:12px;color:var(--text-dim);margin:12px 0 4px}
.debug ul{margin:0;padding-left:18px}
.foot{margin:32px 0 8px;padding-top:12px;border-top:1px solid var(--border);font-size:12px;color:var(--text-dim)}
.lb{position:fixed;inset:0;z-index:50;background:color-mix(in oklab,var(--bg) 96%,transparent);display:flex;flex-direction:column}
.lb[hidden]{display:none}
.lb-bar{display:flex;align-items:center;gap:8px;padding:12px 16px;flex-shrink:0}
.lb-cap{flex:1;min-width:0;font-size:13px;color:var(--text-h);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.lb-cap span{color:var(--text-dim);margin-left:8px;font:12px var(--mono)}
.iconbtn{display:inline-flex;align-items:center;justify-content:center;width:32px;height:32px;border-radius:var(--radius);border:1px solid var(--border-strong);background:var(--bg-elev-1);color:var(--text);cursor:pointer}
.iconbtn:hover{color:var(--text-h);background:var(--bg-elev-2)}
.iconbtn[aria-pressed="true"]{color:var(--accent-h);border-color:var(--accent-border);background:var(--accent-bg)}
.iconbtn:disabled{opacity:.4;cursor:default}
.iconbtn svg{width:16px;height:16px}
.lb-stage{flex:1;min-height:0;overflow:auto;display:flex;padding:0 32px 32px}
.lb-stage img{margin:auto;display:block;flex-shrink:0;border-radius:var(--radius);box-shadow:var(--shadow-pop);background:var(--bg-elev-2)}
.lb-stage.can-zoom img{cursor:zoom-in}
.lb-stage.full img{cursor:zoom-out}
@media (max-width:560px){.cl.grp{grid-template-columns:0 auto auto minmax(0,1fr) auto auto}.si>.g{grid-column:1/-1;padding:0 0 4px}.si>.g:empty{display:none}.si>.st{grid-column:4;justify-self:start;margin:4px 0 0}ul.kv li{grid-template-columns:1fr;gap:2px}}
@media (max-width:640px){.wrap{padding:16px 16px 64px}.banner{padding:14px 16px 6px}.sig .r{grid-template-columns:1fr auto}.sig .r .v{grid-column:1/-1;grid-row:2}.banner{--lbl-w:76px}.player,.txcols{grid-template-columns:1fr}.frow{grid-template-columns:minmax(0,1fr) 32px 56px 52px 48px;gap:8px}}
@media (prefers-reduced-motion:reduce){*{transition:none!important}}`
