package proxy

// dashboardHTML is a small self-contained status dashboard served at "/".
// It polls /status and renders cache and metrics without any external assets.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>DockerFast</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin: 0; font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
         background: #0d1117; color: #c9d1d9; }
  header { padding: 20px 24px; border-bottom: 1px solid #21262d; display: flex; align-items: baseline; gap: 12px; }
  header h1 { font-size: 18px; margin: 0; color: #f0f6fc; }
  header .ver { color: #8b949e; font-size: 12px; }
  main { padding: 24px; max-width: 1100px; margin: 0 auto; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 16px; }
  .card { background: #161b22; border: 1px solid #21262d; border-radius: 8px; padding: 16px; }
  .card .label { color: #8b949e; font-size: 12px; text-transform: uppercase; letter-spacing: .04em; }
  .card .value { font-size: 24px; font-weight: 600; color: #f0f6fc; margin-top: 6px; }
  .card .sub { color: #8b949e; font-size: 12px; margin-top: 4px; }
  h2 { font-size: 14px; color: #8b949e; text-transform: uppercase; letter-spacing: .04em; margin: 32px 0 12px; }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid #21262d; font-size: 13px; }
  th { color: #8b949e; font-weight: 500; }
  .tag { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 11px; border: 1px solid #30363d; color: #8b949e; }
  .tag.default { border-color: #238636; color: #3fb950; }
  .bar { height: 8px; background: #21262d; border-radius: 999px; overflow: hidden; margin-top: 10px; }
  .bar > span { display: block; height: 100%; background: #1f6feb; }
  .err { color: #f85149; }
</style>
</head>
<body>
<header>
  <h1>DockerFast</h1>
  <span class="ver" id="ver"></span>
</header>
<main>
  <div class="grid">
    <div class="card"><div class="label">Cache size</div><div class="value" id="cacheSize">-</div><div class="bar"><span id="cacheBar" style="width:0%"></span></div><div class="sub" id="cacheSub"></div></div>
    <div class="card"><div class="label">Requests</div><div class="value" id="requests">-</div><div class="sub" id="errors"></div></div>
    <div class="card"><div class="label">Blob hits / misses</div><div class="value" id="blobHM">-</div><div class="sub" id="blobRate"></div></div>
    <div class="card"><div class="label">Manifest hits / misses</div><div class="value" id="manifestHM">-</div><div class="sub" id="manifestRate"></div></div>
    <div class="card"><div class="label">Bytes served</div><div class="value" id="served">-</div><div class="sub" id="fetched"></div></div>
    <div class="card"><div class="label">Uptime</div><div class="value" id="uptime">-</div><div class="sub" id="retries"></div></div>
  </div>

  <h2>Upstreams</h2>
  <table><thead><tr><th>Name</th><th>Endpoint</th><th></th></tr></thead><tbody id="upstreams"></tbody></table>
</main>
<script>
function fmtBytes(n) {
  if (n === null || n === undefined) return "-";
  const u = ["B","KiB","MiB","GiB","TiB"]; let i = 0; let v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + " " + u[i];
}
function fmtNum(n) { return (n === null || n === undefined) ? "-" : Number(n).toLocaleString(); }
function fmtDuration(s) {
  s = Math.floor(s); const d = Math.floor(s/86400); s -= d*86400;
  const h = Math.floor(s/3600); s -= h*3600; const m = Math.floor(s/60); s -= m*60;
  if (d > 0) return d + "d " + h + "h";
  if (h > 0) return h + "h " + m + "m";
  if (m > 0) return m + "m " + s + "s";
  return s + "s";
}
function rate(h, m) { const t = h + m; return t === 0 ? "0% hit rate" : (100*h/t).toFixed(1) + "% hit rate"; }
async function refresh() {
  try {
    const r = await fetch("status", { cache: "no-store" });
    if (!r.ok) throw new Error("HTTP " + r.status);
    const d = await r.json();
    document.getElementById("ver").textContent = "v" + d.version;
    document.getElementById("cacheSize").textContent = fmtBytes(d.cache.bytes);
    const pct = d.cache.maxBytes > 0 ? Math.min(100, 100*d.cache.bytes/d.cache.maxBytes) : 0;
    document.getElementById("cacheBar").style.width = pct.toFixed(1) + "%";
    document.getElementById("cacheSub").textContent = fmtNum(d.cache.files) + " files / " + fmtBytes(d.cache.maxBytes) + " (" + pct.toFixed(1) + "%)";
    const mm = d.metrics || {};
    document.getElementById("requests").textContent = fmtNum(mm.dockerfast_requests_total);
    document.getElementById("errors").textContent = fmtNum(mm.dockerfast_errors_total) + " errors";
    const bh = mm.dockerfast_blob_cache_hits_total, bm = mm.dockerfast_blob_cache_misses_total;
    document.getElementById("blobHM").textContent = fmtNum(bh) + " / " + fmtNum(bm);
    document.getElementById("blobRate").textContent = rate(bh, bm) + " (" + fmtNum(mm.dockerfast_cache_bypass_total) + " bypass)";
    const mh = mm.dockerfast_manifest_cache_hits_total, mmiss = mm.dockerfast_manifest_cache_misses_total;
    document.getElementById("manifestHM").textContent = fmtNum(mh) + " / " + fmtNum(mmiss);
    document.getElementById("manifestRate").textContent = rate(mh, mmiss);
    document.getElementById("served").textContent = fmtBytes(mm.dockerfast_bytes_served_total);
    document.getElementById("fetched").textContent = fmtBytes(mm.dockerfast_bytes_fetched_total) + " fetched";
    document.getElementById("uptime").textContent = fmtDuration(d.uptimeSeconds);
    document.getElementById("retries").textContent = fmtNum(mm.dockerfast_retries_total) + " retries";
    const tb = document.getElementById("upstreams"); tb.innerHTML = "";
    (d.upstreams || []).forEach(function (u) {
      const tr = document.createElement("tr");
      const td1 = document.createElement("td"); td1.textContent = u.name;
      if (u.default) { const s = document.createElement("span"); s.className = "tag default"; s.textContent = " default"; td1.appendChild(s); }
      const td2 = document.createElement("td"); td2.textContent = u.endpoint;
      const td3 = document.createElement("td");
      tr.appendChild(td1); tr.appendChild(td2); tr.appendChild(td3);
      tb.appendChild(tr);
    });
  } catch (e) {
    document.getElementById("ver").innerHTML = "<span class=\"err\">connection error: " + e.message + "</span>";
  }
}
refresh();
setInterval(refresh, 3000);
</script>
</body>
</html>
`
