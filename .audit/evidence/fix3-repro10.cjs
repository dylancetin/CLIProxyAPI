// The original repro flow, repeated, with console errors and failed responses recorded per run.
const { chromium } = require("/tmp/usage-pw/node_modules/playwright");
const BASE = process.argv[2];
const RUNS = Number(process.argv[3] || 10);
const SHOT = process.argv[4];
(async () => {
  const results = [];
  const notFound = new Set();
  for (let run = 0; run < RUNS; run++) {
    const browser = await chromium.launch();
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    const errors = [];
    page.on("console", (m) => m.type() === "error" && !m.text().startsWith("Failed to load resource") && errors.push(m.text().slice(0, 300)));
    page.on("pageerror", (e) => errors.push(String(e).slice(0, 300)));
    page.on("response", (r) => r.status() >= 400 && notFound.add(`${r.status()} ${new URL(r.url()).pathname}`));
    await page.route("**/v8/management/observability/usage/**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: "{}" }));
    await page.goto(`${BASE}/management.html`);
    await page.fill('input[name="cpa-management-key"]', "fix2-local-key");
    await page.click('button[type="submit"]');
    await page.waitForSelector("main.main-content", { timeout: 20000 });
    await page.goto(`${BASE}/management.html#/dashboard?cpa-page=usage&cpa-range=24h`);
    await page.waitForTimeout(3000);
    const state = await page.evaluate(() => (document.body.textContent.includes("Unexpected Application Error") ? "crashed" : document.querySelector("[data-cpa-page-root]") ? "ok" : "no-overlay"));
    results.push(errors.length ? `${state}+console-errors` : state);
    if (errors.length) console.log(`run ${run}`, errors);
    if (SHOT && run === RUNS - 1) await page.screenshot({ path: SHOT });
    await browser.close();
  }
  console.log("HTTP>=400 seen:", [...notFound].join(", ") || "none");
  console.log(BASE, results.join(","));
  console.log("crashes:", results.filter((r) => r.startsWith("crashed")).length, "of", results.length);
})();
