(() => {
  "use strict";

  const root = document.documentElement;
  let theme = "dark";
  let view = "ledger";
  try {
    const saved = JSON.parse(localStorage.getItem("cli-proxy-theme"));
    if (["dark", "light", "white", "auto"].includes(saved?.state?.theme)) {
      theme = saved.state.theme;
    } else {
      localStorage.setItem("cli-proxy-theme", JSON.stringify({
        state: { theme: "dark", resolvedTheme: "dark" }, version: 0,
      }));
    }
  } catch {
    // A corrupt preference or unavailable storage must not stop the panel.
    try {
      localStorage.setItem("cli-proxy-theme", JSON.stringify({
        state: { theme: "dark", resolvedTheme: "dark" }, version: 0,
      }));
    } catch { /* Storage can be disabled by the browser. */ }
  }
  const appliedTheme = theme === "auto"
    ? (window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "white")
    : theme;
  if (appliedTheme !== "light") root.setAttribute("data-theme", appliedTheme);
  else root.removeAttribute("data-theme");

  try {
    if (localStorage.getItem("cpa-quota-view") === "cards") view = "cards";
  } catch { /* Keep the default when storage is unavailable. */ }
  root.dataset.cpaQuotaView = view;

  // CPAMC exposes component names in CSS-module classes. Only enhance that
  // known markup; a future/custom layout continues to work without the ledger.
  const part = (component, name) => `[class*="${component}-module__${name}__"]`;
  const cardSelector = `article${part("QuotaCard", "card")}`;
  const rowSelector = part("QuotaBody", "quotaRow");
  const text = (element, selector) => element.querySelector(selector)?.textContent.trim() || "";
  const labels = {
    en: ["Ledger", "Cards", "Quota layout", "Quota summary for this page", "loaded", "of", "Mixed quota windows"],
    "zh-CN": ["列表", "卡片", "额度布局", "本页额度概览", "已加载", "/", "不同额度窗口"],
    "zh-TW": ["列表", "卡片", "額度佈局", "本頁額度概覽", "已載入", "/", "不同額度視窗"],
    ru: ["Список", "Карточки", "Вид квот", "Квоты на этой странице", "загружено", "из", "Разные окна квот"],
    vi: ["Danh sách", "Thẻ", "Bố cục hạn mức", "Hạn mức trên trang này", "đã tải", "trên", "Các kỳ hạn mức khác nhau"],
    ko: ["목록", "카드", "할당량 레이아웃", "이 페이지의 할당량 요약", "로드됨", "/", "서로 다른 할당량 기간"],
  };

  function enhanceQuota() {
    const page = document.querySelector(part("QuotaPage", "page"));
    if (!page) return;
    const words = labels[root.lang] || labels.en;
    const tabs = page.querySelector(part("QuotaPage", "tabsRow"));
    if (tabs && !tabs.querySelector("[data-cpa-view-control]")) {
      const select = document.createElement("select");
      select.dataset.cpaOwned = "";
      select.dataset.cpaViewControl = "";
      select.addEventListener("change", () => {
        root.dataset.cpaQuotaView = select.value;
        try { localStorage.setItem("cpa-quota-view", select.value); } catch { /* Optional persistence. */ }
      });
      tabs.append(select);
    }
    const select = tabs?.querySelector("[data-cpa-view-control]");
    if (select) {
      select.setAttribute("aria-label", words[2]);
      if (select.dataset.language !== root.lang) {
        select.replaceChildren(...["ledger", "cards"].map((value, index) => {
          const option = document.createElement("option");
          option.value = value;
          option.textContent = words[index];
          return option;
        }));
        select.dataset.language = root.lang;
      }
      select.value = root.dataset.cpaQuotaView;
    }

    const grid = page.querySelector(part("QuotaPage", "grid"));
    const cards = grid ? [...grid.querySelectorAll(cardSelector)] : [];
    let summary = page.querySelector("[data-cpa-summary]");
    if (!cards.length) {
      summary?.remove();
      return;
    }
    const groups = new Map();
    let previousProvider = "";
    for (const card of cards) {
      const identity = card.querySelector(part("QuotaCard", "iconWrap"));
      const provider = identity?.title;
      if (!provider) continue;
      card.dataset.cpaProvider = provider;
      card.toggleAttribute("data-cpa-provider-start", previousProvider !== provider);
      previousProvider = provider;
      if (!groups.has(provider)) groups.set(provider, { provider, icon: identity.querySelector("img")?.getAttribute("src"), entries: [] });
      // Aggregate only the first real percentage window, never plan balances,
      // placeholders, error messages, or unknown quota. Do not query upstream.
      const row = [...card.querySelectorAll(rowSelector)].find((candidate) => candidate.querySelector(part("QuotaBody", "quotaPercent")));
      const label = row ? text(row, part("QuotaBody", "quotaModel")) : "";
      const percentText = row ? text(row, part("QuotaBody", "quotaPercent")) : "";
      const parsed = /^\d+(?:\.\d+)?%$/.test(percentText) ? Number.parseFloat(percentText) : null;
      const percent = parsed !== null && parsed >= 0 && parsed <= 100 ? parsed : null;
      groups.get(provider).entries.push({ label, percent });
    }
    if (!groups.size) return;
    const model = JSON.stringify([root.lang, [...groups.values()]]);
    if (summary?.dataset.model === model) return;
    if (!summary) {
      summary = document.createElement("section");
      summary.dataset.cpaOwned = "";
      summary.dataset.cpaSummary = "";
      grid.before(summary);
    }
    summary.setAttribute("aria-label", words[3]);
    summary.dataset.model = model;
    const tiles = [];
    for (const { provider, icon, entries } of groups.values()) {
      const tile = document.createElement("div");
      tile.className = "cpa-provider-summary";
      const heading = document.createElement("div");
      heading.className = "cpa-provider-heading";
      if (icon) {
        const image = document.createElement("img");
        image.src = icon;
        image.alt = "";
        heading.append(image);
      }
      const title = document.createElement("strong");
      title.textContent = provider;
      const count = document.createElement("span");
      const loaded = entries.filter((entry) => entry.percent !== null);
      count.textContent = `${loaded.length}/${entries.length} ${words[4]}`;
      heading.append(title, count);
      const windowLabel = document.createElement("div");
      windowLabel.className = "cpa-summary-label";
      const comparable = new Set(loaded.map((entry) => entry.label)).size <= 1;
      windowLabel.textContent = comparable ? (loaded[0]?.label || "—") : words[6];
      const amount = document.createElement("div");
      amount.className = "cpa-summary-amount";
      const value = document.createElement("strong");
      value.textContent = loaded.length && comparable ? `${Math.round(loaded.reduce((sum, entry) => sum + entry.percent, 0))}%` : "—";
      amount.append(value);
      if (loaded.length && comparable) {
        const capacity = document.createElement("span");
        capacity.textContent = `${words[5]} ${loaded.length * 100}%`;
        amount.append(capacity);
      }
      const segments = document.createElement("div");
      segments.className = "cpa-summary-segments";
      segments.setAttribute("aria-hidden", "true");
      for (const { percent } of entries) {
        const segment = document.createElement("span");
        const fill = document.createElement("i");
        fill.style.width = `${percent ?? 0}%`;
        fill.dataset.level = percent === null ? "unknown" : percent < 20 ? "low" : percent < 70 ? "medium" : "high";
        segment.append(fill);
        segments.append(segment);
      }
      tile.append(heading, windowLabel, amount, segments);
      tiles.push(tile);
    }
    summary.replaceChildren(...tiles);
  }

  function start() {
    const app = document.getElementById("root");
    if (!app) return;
    let pending = false;
    const schedule = () => {
      if (pending) return;
      pending = true;
      requestAnimationFrame(() => {
        pending = false;
        enhanceQuota();
      });
    };
    new MutationObserver((mutations) => {
      if (mutations.some((mutation) => !mutation.target.closest?.("[data-cpa-owned]"))) schedule();
    }).observe(app, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ["style", "title"] });
    new MutationObserver(schedule).observe(root, { attributes: true, attributeFilter: ["lang"] });
    schedule();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start, { once: true });
  else start();
})();
