// Navigation flows around the usage page; reports crashes, console errors, and host-hiding state.
const { chromium } = require("/tmp/usage-pw/node_modules/playwright");
const BASE = process.argv[2];

const probe = () => {
  const main = document.querySelector("main.main-content");
  const host = main?.querySelector(":scope > .page-transition");
  const section = document.querySelector("[data-cpa-page-root]");
  const content = document.querySelector(".content");
  const hit = section ? document.elementFromPoint(760, Math.min(innerHeight - 10, section.getBoundingClientRect().top + 200)) : null;
  const hs = host && getComputedStyle(host);
  return {
    hash: location.hash,
    crashed: document.body.textContent.includes("Unexpected Application Error"),
    overlay: !!section,
    cpaPage: document.documentElement.dataset.cpaPage || null,
    host: hs ? `${hs.visibility}/${hs.position}/${hs.clipPath}` : null,
    hostBox: host ? `${Math.round(host.getBoundingClientRect().width)}x${Math.round(host.getBoundingClientRect().height)}` : null,
    sectionTop: section ? Math.round(section.getBoundingClientRect().top) : null,
    hitInSection: section ? !!hit?.closest("[data-cpa-page-root]") : null,
    scroll: content ? `${content.scrollTop}/${content.scrollHeight}/${content.clientHeight}` : null,
    sectionHeight: section ? Math.round(section.getBoundingClientRect().height) : null,
    activeNav: [...document.querySelectorAll(".sidebar a.nav-item.active")].map((a) => a.getAttribute("href")).join(" "),
  };
};

async function login(browser, errors, remember = false) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  page.on("console", (m) => m.type() === "error" && !m.text().startsWith("Failed to load resource") && errors.push(m.text().slice(0, 300)));
  page.on("pageerror", (e) => errors.push(String(e).slice(0, 300)));
  await page.route("**/v8/management/observability/usage/**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: "{}" }));
  await page.goto(`${BASE}/management.html`);
  await page.fill('input[name="cpa-management-key"]', "fix2-local-key");
  if (remember) await page.click('form label:has(input[type="checkbox"])');
  await page.click('button[type="submit"]');
  await page.waitForSelector("main.main-content", { timeout: 20000 });
  return page;
}

(async () => {
  const browser = await chromium.launch();
  const report = [];
  const step = async (page, name, errors) => {
    const p = await page.evaluate(probe);
    report.push({ name, ...p, errors: errors.splice(0) });
    console.log(JSON.stringify(report.at(-1)));
  };

  // Settled dashboard, nav link, reload, Back, Forward.
  {
    const errors = [];
    const page = await login(browser, errors, true);
    await page.waitForTimeout(2500);
    await page.click('a[data-cpa-nav="usage"]');
    await page.waitForTimeout(80);
    const early = await page.evaluate(probe);
    await page.waitForTimeout(1500);
    report.push({ name: "nav-link (80ms after click)", ...early, errors: [] });
    console.log(JSON.stringify(report.at(-1)));
    await step(page, "nav-link (settled)", errors);
    await page.reload();
    await page.waitForTimeout(3000);
    console.log("after reload: login form shown =", await page.locator('input[name="cpa-management-key"]').count() > 0);
    await step(page, "reload on usage", errors);
    await page.goBack();
    await page.waitForTimeout(1500);
    await step(page, "back", errors);
    await page.goForward();
    await page.waitForTimeout(1500);
    await step(page, "forward", errors);
    await page.click('.sidebar a.nav-item[href$="/logs"]');
    await page.waitForTimeout(1500);
    await step(page, "logs via nav", errors);
    await page.goBack();
    await page.waitForTimeout(1500);
    await step(page, "back to usage from logs", errors);
    await page.close();
  }
  // Direct route after login, then leave within the reveal window.
  for (const [name, leave] of [
    ["direct then Logs at 150ms", (page) => page.click('.sidebar a.nav-item[href$="/logs"]')],
    ["direct then Back at 150ms", (page) => page.goBack()],
  ]) {
    const errors = [];
    const page = await login(browser, errors);
    await page.goto(`${BASE}/management.html#/dashboard?cpa-page=usage`);
    await page.waitForTimeout(150);
    await leave(page);
    await page.waitForTimeout(2000);
    await step(page, name, errors);
    await page.close();
  }
  await browser.close();
  const bad = report.filter((r) => r.crashed || r.errors.length);
  console.log(bad.length ? `FAIL ${bad.map((r) => r.name).join(", ")}` : `PASS ${report.length} steps, no error boundary, no console errors`);
})();
