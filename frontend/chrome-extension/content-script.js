(() => {
  if (globalThis.ghosthandDOMBridgeLoaded) return;
  globalThis.ghosthandDOMBridgeLoaded = true;
  const endpoint = "ghosthand.dom";
  const ids = new WeakMap();
  const elements = new Map();
  const waits = new Map();
  const encoder = new TextEncoder();
  let nextId = 0;

  function shorten(value, size = 200) {
    return String(value ?? "").replace(/\s+/g, " ").trim().slice(0, size);
  }

  function clipLargeText(value, maxChars = 60000) {
    const text = String(value ?? "");
    return text.length > maxChars ? text.slice(0, maxChars) + "\n...[truncated]" : text;
  }

  function visible(el) {
    if (!el.getClientRects().length || el.closest("[hidden],[inert],[aria-hidden='true']")) return false;
    const style = getComputedStyle(el);
    return style.visibility === "visible" && style.display !== "none" && style.opacity !== "0";
  }

  function roots() {
    const result = [document];
    for (const root of result) {
      for (const el of root.querySelectorAll("*")) {
        if (el.shadowRoot) result.push(el.shadowRoot);
      }
    }
    return result;
  }

  function xpath(el) {
    if (el.getRootNode() !== document) return null;
    const parts = [];
    for (let node = el; node; node = node.parentElement) {
      const tag = node.localName;
      let index = 1;
      for (let sibling = node.previousElementSibling; sibling; sibling = sibling.previousElementSibling) {
        if (sibling.localName === tag) index++;
      }
      parts.unshift((node.namespaceURI === "http://www.w3.org/1999/xhtml" ? tag : "*[local-name()='" + tag + "']") + "[" + index + "]");
    }
    return "/" + parts.join("/");
  }

  function identify(el) {
    let id = ids.get(el);
    if (!id) {
      id = ++nextId;
      ids.set(el, id);
      elements.set(id, new WeakRef(el));
    }
    if (el.getAttribute("data-gh-id") !== String(id)) el.setAttribute("data-gh-id", id);
    return id;
  }

  function rect(el) {
    const {x, y, width, height} = el.getBoundingClientRect();
    return {x, y, width, height};
  }

  function value(el) {
    if (el instanceof HTMLInputElement && el.type === "password") return null;
    if (el instanceof HTMLSelectElement && el.multiple) return Array.from(el.selectedOptions, option => option.value);
    return el.value ?? (el.isContentEditable ? el.innerText : null);
  }

  function label(el) {
    return el.getAttribute("aria-label") || el.innerText || Array.from(el.labels ?? [], item => item.innerText).join(" ") ||
      el.getAttribute("alt") || el.getAttribute("title") || el.textContent;
  }

  function describe(el, compact = false) {
    const id = identify(el);
    const path = xpath(el);
    const current = value(el);
    return {
      gh_id: id,
      xpath: compact && path?.length > 320 ? null : path,
      selector: '[data-gh-id="' + id + '"]',
      tag: shorten(el.localName, 40), type: shorten(el.getAttribute("type"), 40) || null, role: shorten(el.getAttribute("role"), 40) || null,
      text: shorten(label(el)), placeholder: shorten(el.getAttribute("placeholder")),
      value: Array.isArray(current) ? current.slice(0, 8).map(item => shorten(item)) : current == null ? null : shorten(current),
      checked: typeof el.checked === "boolean" ? el.checked : undefined,
      rect: rect(el)
    };
  }

  const interactive = [
    "a[href]", "button", "input:not([type=hidden])", "textarea", "select", "summary", "label", "iframe", "frame", "[onclick]",
    "[tabindex]:not([tabindex='-1'])", "[contenteditable]:not([contenteditable=false])",
    "[role=button]", "[role=link]", "[role=checkbox]", "[role=radio]", "[role=tab]", "[role=option]", "[role=menuitem]",
    "[role=textbox]", "[role=combobox]", "[role=switch]", "[role=slider]", "[role=searchbox]"
  ].join(",");

  function inspect(data) {
    if (window !== window.top && (!innerWidth || !innerHeight)) return snapshot([], false, data);
    for (const [id, ref] of elements) {
      if (!ref.deref()?.isConnected) elements.delete(id);
    }
    const limit = Math.min(400, data.max_elements ?? 150);
    const filter = String(data.filter_text ?? "").toLocaleLowerCase();
    const found = [];
    let size = 0;
    for (const root of roots()) {
      for (const el of root.querySelectorAll(interactive)) {
        if (!visible(el) || (filter && ![label(el), el.getAttribute("placeholder"), el.getAttribute("role"), value(el)]
          .some(item => shorten(item, 1000).toLocaleLowerCase().includes(filter)))) continue;
        if (found.length === limit) return snapshot(found, true, data);
        const item = describe(el, true);
        size += encoder.encode(JSON.stringify(item)).length + 1;
        if (size > 170000) return snapshot(found, true, data);
        found.push(item);
      }
    }
    return snapshot(found, false, data);
  }

  function snapshot(found, truncated, data) {
    return {
      url: location.href.slice(0, 4096), title: document.title.slice(0, 512), scroll: {x: scrollX, y: scrollY}, elements: found, truncated,
      text: data.include_text === false ? undefined : shorten(document.body?.innerText, 1500)
    };
  }

  function locate(data, onlyLinks = false) {
    if (data.gh_id != null) {
      const id = Number(data.gh_id);
      const el = elements.get(id)?.deref();
      if (el?.isConnected) return [el];
      return roots().flatMap(root => Array.from(root.querySelectorAll('[data-gh-id="' + id + '"]')));
    }
    if (data.xpath) {
      const result = document.evaluate(data.xpath, document, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
      return Array.from({length: result.snapshotLength}, (_, i) => result.snapshotItem(i)).filter(item => item instanceof Element);
    }
    if (data.selector) return roots().flatMap(root => Array.from(root.querySelectorAll(data.selector)));
    if (data.text) {
      const query = shorten(data.text, 1000).toLocaleLowerCase();
      const candidates = roots().flatMap(root => Array.from(root.querySelectorAll(onlyLinks ? "a[href]" :
        interactive + ",p,li,td,th,h1,h2,h3,h4,h5,h6,span")));
      const matches = candidates.filter(el => visible(el) && shorten(label(el), 2000).toLocaleLowerCase().includes(query));
      const exact = matches.filter(el => shorten(label(el), 2000).toLocaleLowerCase() === query);
      const preferred = (exact.length ? exact : matches).filter(el => el.matches(interactive));
      return preferred.length ? preferred : (exact.length ? exact : matches);
    }
    throw new Error("需要 gh_id、xpath、selector 或 text 定位元素");
  }

  function target(data, onlyLinks = false) {
    const el = locate(data, onlyLinks)[0];
    if (!el) throw new Error("找不到目标元素");
    return el;
  }

  function read(data, getter) {
    const found = locate(data).slice(0, 400);
    if (data.all) return found.map(getter);
    if (!found.length) throw new Error("找不到目标元素");
    return getter(found[0]);
  }

  function wait(data, signal) {
    const state = data.state || "appear";
    const matches = () => {
      if (state === "complete") return document.readyState === "complete";
      const found = locate(data);
      if (state === "text_contains") return found.some(el => (el.innerText ?? el.textContent ?? "").includes(data.text_contains));
      return state === "disappear" ? found.length === 0 : found.length > 0;
    };
    return new Promise((resolve, reject) => {
      const observer = new MutationObserver(check);
      const timer = setTimeout(() => finish(new Error("等待条件超时")), data.timeout_ms ?? 10000);
      const poll = setInterval(check, 100);
      function abort() { finish(new Error("浏览器操作已取消")); }
      function finish(error) {
        observer.disconnect();
        clearTimeout(timer);
        clearInterval(poll);
        signal.removeEventListener("abort", abort);
        if (error) reject(error);
        else resolve({matched: true, state});
      }
      function check() {
        try { if (matches()) finish(); }
        catch (error) { finish(error); }
      }
      signal.addEventListener("abort", abort, {once: true});
      observer.observe(document, {attributes: true, childList: true, characterData: true, subtree: true});
      if (signal.aborted) abort();
      else check();
    });
  }

  function bringIntoView(el) {
    el.scrollIntoView({behavior: "instant", block: "center", inline: "nearest"});
  }

  function mouse(el, names) {
    const {x, y, width, height} = rect(el);
    for (const type of names) {
      el.dispatchEvent(new MouseEvent(type, {
        view: window, bubbles: type !== "mouseenter", cancelable: true, composed: true,
        clientX: x + width / 2, clientY: y + height / 2, buttons: type === "mousedown" ? 1 : 0
      }));
    }
  }

  function fill(el, data) {
    const contentEditable = el.isContentEditable;
    const host = contentEditable ? el.closest("[contenteditable]") ?? el : el;
    const proto = host instanceof HTMLInputElement ? HTMLInputElement.prototype :
      host instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : null;
    if (!proto && !contentEditable) throw new Error("目标不是可编辑元素");
    if (host.disabled || host.readOnly) throw new Error("目标字段不可编辑");
    const text = String(data.value ?? "");
    const set = value => {
      if (contentEditable) host.textContent = value;
      else Object.getOwnPropertyDescriptor(proto, "value").set.call(host, value);
    };
    const input = (kind, payload) => host.dispatchEvent(new InputEvent("input", {
      inputType: kind, data: payload, bubbles: true, composed: true
    }));
    bringIntoView(host);
    host.focus();
    const key = (type, key, code) => host.dispatchEvent(new KeyboardEvent(type, {key, code, bubbles: true, cancelable: true, composed: true}));
    const last = Array.from(text).at(-1) ?? "Unidentified";
    key("keydown", last);
    if (data.clear_first) {
      set("");
      input("deleteContentBackward", null);
    }
    set(text);
    input("insertText", text);
    host.dispatchEvent(new Event("change", {bubbles: true}));
    key("keyup", last);
    if (data.submit) {
      const unhandled = key("keydown", "Enter", "Enter");
      key("keyup", "Enter", "Enter");
      if (unhandled && host instanceof HTMLInputElement && host.form) {
        const submitter = Array.from(host.form.elements).find(control =>
          (control instanceof HTMLButtonElement || control instanceof HTMLInputElement) && control.type === "submit" && !control.disabled);
        host.form.requestSubmit(submitter);
      }
    }
    return {filled: true};
  }

  function selectElement(el) {
    if (!(el instanceof HTMLSelectElement)) throw new Error("目标不是 select 元素");
    return el;
  }

  function select(el, data) {
    selectElement(el);
    if (el.disabled) throw new Error("目标下拉框不可编辑");
    const options = Array.from(el.options);
    const values = Array.isArray(data.value) ? data.value.map(String) : [String(data.value)];
    const selected = Number.isInteger(data.index) ? [options[data.index]] :
      values.map(item => options.find(option => option.value === item) ?? options.find(option => option.text === item));
    if (!selected.length || selected.some(option => !option || option.disabled || option.closest("optgroup[disabled]"))) {
      throw new Error("找不到可选择的选项");
    }
    if (!el.multiple && selected.length > 1) throw new Error("目标下拉框只允许单选");
    if (el.multiple) {
      const setter = Object.getOwnPropertyDescriptor(HTMLOptionElement.prototype, "selected").set;
      for (const option of options) setter.call(option, selected.includes(option));
    } else {
      Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "selectedIndex").set.call(el, selected[0].index);
    }
    el.dispatchEvent(new Event("input", {bubbles: true, composed: true}));
    el.dispatchEvent(new Event("change", {bubbles: true}));
    return value(el);
  }

  function checkbox(el, data, write) {
    const desired = data.value === undefined ? true : Boolean(data.value);
    if (el instanceof HTMLInputElement && ["radio", "checkbox"].includes(el.type)) {
      if (write && el.checked !== desired) {
        if (el.disabled) throw new Error("目标选项不可编辑");
        if (el.type === "radio" && !desired) {
          Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "checked").set.call(el, false);
          el.dispatchEvent(new Event("input", {bubbles: true, composed: true}));
          el.dispatchEvent(new Event("change", {bubbles: true}));
        } else {
          el.click();
        }
      }
      return {checked: el.checked};
    }
    const role = el.getAttribute("role");
    if (["checkbox", "radio", "switch"].includes(role)) {
      const current = el.getAttribute("aria-checked") === "true";
      if (write && current !== desired) {
        if (el.matches(":disabled,[aria-disabled='true']")) throw new Error("目标选项不可编辑");
        clickElement(el);
      }
      return {checked: el.getAttribute("aria-checked") === "true"};
    }
    throw new Error("目标不是单选框或复选框");
  }

  function clickElement(el) {
    bringIntoView(el);
    mouse(el, ["mouseover", "mousedown", "mouseup"]);
    if (typeof el.click === "function") el.click();
    else mouse(el, ["click"]);
  }

  function similar(el) {
    let branch = el;
    const path = [];
    const listItem = el.closest("tr,li,[role=row],[role=listitem]");
    let withinItem = listItem && listItem !== el;
    while (branch.parentElement && branch.parentElement !== document.body) {
      const matches = Array.from(branch.parentElement.children).filter(node => node.localName === branch.localName);
      if (!withinItem && matches.length > 1) {
        return matches.slice(0, 80).map(node => path.reduceRight((parent, step) =>
          parent && Array.from(parent.children).filter(child => child.localName === step.tag)[step.index], node))
          .filter(Boolean).map(node => describe(node, true));
      }
      path.push({tag: branch.localName, index: matches.indexOf(branch)});
      branch = branch.parentElement;
      if (branch === listItem) withinItem = false;
    }
    return [describe(el, true)];
  }

  async function screenshot(el) {
    bringIntoView(el);
    if (!globalThis.html2canvas) throw new Error("元素截图库未加载");
    const {width, height} = rect(el);
    if (!width || !height) throw new Error("元素没有可截图区域");
    const scale = Math.min(devicePixelRatio, 2000 / Math.max(width, height), Math.sqrt(1600000 / (width * height)));
    const canvas = await html2canvas(el, {scale, logging: false, useCORS: true, backgroundColor: "#fff",
      ignoreElements: node => node instanceof HTMLInputElement && node.type === "password"});
    return {jpeg: canvas.toDataURL("image/jpeg", 0.82)};
  }

  const actions = {
    "Chrome.InspectDOM": inspect,
    "Chrome.GetTitle": () => document.title,
    "Chrome.GetUrl": () => location.href,
    "Chrome.GetViewSource": () => clipLargeText(document.documentElement?.outerHTML ?? ""),
    "Chrome.Exists": data => locate(data).length > 0,
    "Chrome.Wait": wait,
    "Chrome.GetText": data => read(data, el => clipLargeText(el.innerText ?? el.textContent, data.all ? 4000 : 60000)),
    "Chrome.GetValue": data => read(data, value),
    "Chrome.GetinnerHTML": data => read(data, el => clipLargeText(el.outerHTML, data.all ? 4000 : 60000)),
    "Chrome.GetAttributes": data => read(data, el => Object.fromEntries(
      (data.attributes ?? (data.attribute ? [data.attribute] : el.getAttributeNames())).map(name => [name, el.getAttribute(name)]))),
    "Chrome.GetRectangleByXpath": data => read(data, rect),
    "Chrome.GetParentElement": data => target(data).parentElement ? describe(target(data).parentElement) : null,
    "Chrome.GetChildElement": data => Array.from(target(data).children).slice(0, 80).map(el => describe(el, true)),
    "Chrome.GetSiblingElement": data => {
      const el = target(data);
      return Array.from(el.parentElement?.children ?? []).filter(node => node !== el).slice(0, 80).map(node => describe(node, true));
    },
    "Chrome.GetSimilar": data => similar(target(data)),
    "Chrome.Click": data => {
      const el = target(data);
      if (el.matches(":disabled")) throw new Error("目标元素已禁用");
      clickElement(el);
      return {clicked: true};
    },
    "Chrome.ClickByLinkText": data => {
      const el = target(data, true);
      if (!el.matches("a[href]")) throw new Error("目标不是链接");
      clickElement(el);
      return {clicked: true};
    },
    "Chrome.SetValue": data => fill(target(data), data),
    "Chrome.GetSelect": data => read(data, el => value(selectElement(el))),
    "Chrome.SetSelect": data => select(target(data), data),
    "Chrome.GetSelectOptions": data => Array.from(selectElement(target(data)).options, option =>
      ({value: option.value, text: option.text, selected: option.selected, disabled: option.disabled})),
    "Chrome.setRadioCheckBox": data => checkbox(target(data), data, true),
    "Chrome.getRadioCheckBoxStatus": data => checkbox(target(data), data, false),
    "Chrome.ScrollToView": data => { bringIntoView(target(data)); return {x: scrollX, y: scrollY}; },
    "Chrome.ScrollPage": data => {
      const distance = data.distance ?? (data.pages ?? 1) * (["left", "right"].includes(data.direction) ? innerWidth : innerHeight);
      const direction = data.direction ?? "down";
      scrollBy({left: direction === "left" ? -distance : direction === "right" ? distance : 0,
        top: direction === "up" ? -distance : direction === "down" ? distance : 0, behavior: "instant"});
      return {x: scrollX, y: scrollY};
    },
    "Chrome.Focus": data => { target(data).focus(); return {focused: true}; },
    "Chrome.Hover": data => {
      const el = target(data);
      bringIntoView(el);
      mouse(el, ["mouseover", "mouseenter", "mousemove"]);
      return {hovered: true};
    },
    "Chrome.ScreenShotToBase64": data => screenshot(target(data)),
    "Chrome.SwitchToFrame": data => {
      const frame = target(data);
      if (!frame.matches("iframe,frame") || !frame.contentWindow) throw new Error("目标不是可访问的 iframe");
      frame.contentWindow.postMessage({ghosthandFrameProbe: data.probe}, "*");
      return true;
    }
  };

  if (window !== window.top) {
    window.addEventListener("message", event => {
      if (event.source !== window.parent || typeof event.data?.ghosthandFrameProbe !== "string") return;
      chrome.runtime.sendMessage({ghosthandFrameProbe: event.data.ghosthandFrameProbe}).catch(() => {});
    });
  }

  chrome.runtime.onMessage.addListener((message, sender, reply) => {
    if (sender.id !== chrome.runtime.id || message?.endpoint !== endpoint) return false;
    if (message.code === "Browser.cancel") {
      waits.get(message.operation)?.abort();
      return false;
    }
    const action = Object.hasOwn(actions, message.code) ? actions[message.code] : null;
    if (!action) return false;
    const controller = new AbortController();
    if (message.code === "Chrome.Wait") waits.set(message.operation, controller);
    Promise.resolve().then(() => action(message.data ?? {}, controller.signal))
      .then(data => reply({ok: true, data}), error => reply({ok: false, error: error.message || String(error)}))
      .finally(() => waits.delete(message.operation));
    return true;
  });
})();
