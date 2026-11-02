// Logs in, then jumps straight to the usage hash, and reports whether the host panel crashed.
const { chromium } = require("/tmp/usage-pw/node_modules/playwright");
const BASE = process.argv[2];
(async () => {
  const results = [];
  for (let run = 0; run < 3; run++) {
    const browser = await chromium.launch();
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    await page.route("**/v8/management/observability/usage/**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: "{}" }));
    await page.goto(`${BASE}/management.html`);
    await page.fill('input[name="cpa-management-key"]', "fix2-local-key");
    await page.click('button[type="submit"]');
    await page.waitForSelector("main.main-content", { timeout: 20000 });
    await page.goto(`${BASE}/management.html#/dashboard?cpa-page=usage&cpa-range=24h`);
    await page.waitForTimeout(3000);
    results.push(await page.evaluate(() => (document.body.textContent.includes("Unexpected Application Error") ? "crashed" : document.querySelector("[data-cpa-page-root]") ? "ok" : "no-overlay")));
    await browser.close();
  }
  console.log(BASE, results.join(","));
})();
