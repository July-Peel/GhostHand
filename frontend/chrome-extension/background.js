const host = "com.ghosthand.nativemessage";
const browser = /Edg\//.test(navigator.userAgent) ? "edge" : "chrome";
const heartbeat = "ghosthand.reconnect";
const activeRequests = new Map();
const selectedFrames = new Map();
const probes = new Map();
const maxResultBytes = 256 * 1024;
const encoder = new TextEncoder();
let native;
let pingTimer;
let reconnectTimer;
let reconnectDelay = 1000;
let uninstalling = false;

chrome.alarms.onAlarm.addListener(alarm => {
  if (alarm.name === heartbeat) connect();
});
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.tabs.onRemoved.addListener(tabId => selectedFrames.delete(tabId));
chrome.webNavigation.onCommitted.addListener(({tabId, frameId}) => {
  if (frameId === 0 || selectedFrames.get(tabId) === frameId) selectedFrames.delete(tabId);
});

chrome.runtime.onMessage.addListener((message, sender) => {
  const probe = probes.get(message?.ghosthandFrameProbe);
  if (!probe || sender.id !== chrome.runtime.id || sender.tab?.id !== probe.tabId ||
    !probe.children.has(sender.frameId)) return false;
  probe.resolve(sender.frameId);
  return false;
});

async function ensureAlarm() {
  if (!await chrome.alarms.get(heartbeat)) await chrome.alarms.create(heartbeat, {periodInMinutes: 0.5});
}

function connect() {
  if (native || uninstalling) return;
  clearTimeout(reconnectTimer);
  const port = chrome.runtime.connectNative(host);
  native = port;
  port.onDisconnect.addListener(() => {
    const error = chrome.runtime.lastError?.message;
    if (native !== port) return;
    native = undefined;
    clearInterval(pingTimer);
    for (const controller of activeRequests.values()) controller.abort();
    activeRequests.clear();
    if (!uninstalling) {
      if (error) console.debug("GhostHand Native Messaging:", error);
      reconnectTimer = setTimeout(connect, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 5000);
    }
  });
  port.onMessage.addListener(request => {
    if (request.code === "Browser.ping" && !request.requestId) {
      reconnectDelay = 1000;
      return;
    }
    if (request.code === "Browser.cancel") {
      activeRequests.get(request.requestId)?.abort();
      return;
    }
    if (request.requestId && typeof request.code === "string") void respond(port, request);
  });
  void activeTab().catch(() => null).then(tab => post(port, {
    Code: "Browser.connected", Status: true,
    Data: {extensionId: chrome.runtime.id, browser, active_tab: tab ? {tab_id: tab.id, window_id: tab.windowId,
      title: tab.title?.slice(0, 200), url: tab.url?.slice(0, 500)} : null}
  }));
  pingTimer = setInterval(() => post(port, {Code: "Browser.ping", Status: true}), 20000);
}

function post(port, message) {
  if (port === native) {
    try { port.postMessage(message); }
    catch (error) { console.debug("GhostHand response:", error.message); }
  }
}

async function respond(port, request) {
  const {requestId, code} = request;
  const args = request.data ?? {};
  const controller = new AbortController();
  const signal = controller.signal;
  activeRequests.set(requestId, controller);
  const context = {tabId: request.tabId > 0 ? request.tabId : undefined, frameId: request.frameId, signal};
  try {
    const data = await execute(code, args, context);
    signal.throwIfAborted();
    const isScreenshot = code === "Chrome.CaptureVisibleTab" || code === "Chrome.ScreenShotToBase64";
    const size = encoder.encode(JSON.stringify(data ?? null)).length;
    if (size > (isScreenshot ? 7 * 1024 * 1024 : maxResultBytes - 19000)) {
      throw new Error("浏览器结果超出大小上限，请缩小提取范围");
    }
    const tab = context.tabId === undefined ? null : await chrome.tabs.get(context.tabId).catch(() => null);
    post(port, {requestId, Code: code, Status: true, TabId: tab?.id, Url: tab?.url?.slice(0, 4096), Title: tab?.title?.slice(0, 512), Data: data ?? null});
  } catch (error) {
    const details = error.batch;
    post(port, {
      requestId, Code: code, Status: false, Error: (error.message || String(error)).slice(0, 160), Data: details
    });
  } finally {
    if (activeRequests.get(requestId) === controller) activeRequests.delete(requestId);
  }
}

const activeTab = async () => {
  const [tab] = await chrome.tabs.query({active: true, lastFocusedWindow: true});
  if (!tab?.id) throw new Error("找不到活动标签页");
  return tab;
};
const tabInfo = tab => ({tab_id: tab.id, window_id: tab.windowId, index: tab.index,
  title: tab.title?.slice(0, 512) ?? "", url: tab.url?.slice(0, 4096) ?? "", active: tab.active, status: tab.status});

function checkedUrl(raw, file = true) {
  let url;
  try { url = new URL(raw); }
  catch { throw new Error("网址无效"); }
  if (!["http:", "https:", ...(file ? ["file:"] : [])].includes(url.protocol) || url.username || url.password) {
    throw new Error("此网址协议或凭据格式不受支持");
  }
  return url;
}

function sleep(ms, signal) {
  signal.throwIfAborted();
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", abort);
      resolve();
    }, ms);
    function abort() {
      clearTimeout(timer);
      reject(signal.reason);
    }
    signal.addEventListener("abort", abort, {once: true});
  });
}

async function resolveTab(args, context) {
  const hasFilter = args.index !== undefined || args.title_contains || args.title_regex || args.url_contains;
  const id = args.tab_id ?? (hasFilter ? undefined : context.tabId);
  if (id !== undefined) return chrome.tabs.get(id);
  if (!hasFilter) return activeTab();
  const tabs = await chrome.tabs.query(args.index === undefined ? {} : {lastFocusedWindow: true});
  const regex = args.title_regex ? new RegExp(args.title_regex, "i") : null;
  const found = tabs.find(tab => (args.index === undefined || args.index === tab.index) &&
    (!args.title_contains || tab.title?.toLocaleLowerCase().includes(args.title_contains.toLocaleLowerCase())) &&
    (!args.url_contains || tab.url?.includes(args.url_contains)) && (!regex || regex.test(tab.title ?? "")));
  if (!found) throw new Error("找不到符合条件的标签页");
  return found;
}

async function waitComplete(id, signal, deadline, soft = false) {
  while (true) {
    signal.throwIfAborted();
    const tab = await chrome.tabs.get(id);
    if (tab.status === "complete" && !tab.pendingUrl) return tab;
    if (Date.now() >= deadline) {
      if (soft) return tab;
      throw new Error("等待页面加载超时");
    }
    await sleep(100, signal);
  }
}

async function waitForCommit(id, before, url, signal, deadline) {
  while (true) {
    signal.throwIfAborted();
    const tab = await chrome.tabs.get(id);
    const frame = await chrome.webNavigation.getFrame({tabId: id, frameId: 0}).catch(() => null);
    if (frame && !tab.pendingUrl && frame.url === tab.url &&
      (before ? frame.documentId !== before.documentId || frame.url !== before.url || frame.url === url :
        !frame.url.startsWith("about:") && !/^(edge|chrome):/.test(frame.url))) return tab;
    if (Date.now() >= deadline) throw new Error("等待浏览器提交导航超时");
    await sleep(75, signal);
  }
}

async function navigation(code, args, context) {
  if (code === "Chrome.ListTabs") return (await chrome.tabs.query({})).map(tabInfo);
  if (code === "Chrome.CloseExtensionsTabs") {
    const tabs = await chrome.tabs.query({});
    const management = tabs.filter(tab => {
      try {
        const url = new URL(tab.url ?? "");
        return ["chrome:", "edge:"].includes(url.protocol) && url.hostname === "extensions" &&
          (args.install_token ? url.searchParams.get("ghosthand") === args.install_token :
            url.searchParams.has("ghosthand"));
      } catch { return false; }
    });
    const ids = management.map(tab => tab.id);
    for (const windowId of new Set(management.map(tab => tab.windowId))) {
      // A freshly launched browser must keep a window open after its setup tab is closed.
      if (tabs.every(tab => tab.windowId !== windowId || ids.includes(tab.id))) {
        await chrome.tabs.create({windowId, active: true});
      }
    }
    if (ids.length) await chrome.tabs.remove(ids);
    return {closed: ids.length};
  }
  const open = code === "Chrome.OpenUrl" || code === "Chrome.JumpUrl";
  const waitForNavigation = args.wait_complete !== false;
  const strictComplete = args.wait_complete === true;
  const deadline = Date.now() + (args.timeout_ms ?? 10000);
  const url = open ? checkedUrl(args.url).href : undefined;
  const openInNewTab = open && (args.new_tab ?? args.newTab ?? (code === "Chrome.OpenUrl" && !context.inBatch && args.tab_id === undefined));
  if (openInNewTab) {
    const anchorId = args.tab_id ?? context.tabId;
    const windowId = anchorId === undefined ? undefined : (await chrome.tabs.get(anchorId)).windowId;
    let tab = await chrome.tabs.create({url, active: true, windowId});
    context.tabId = tab.id;
    context.frameId = undefined;
    if (waitForNavigation) tab = await waitForCommit(tab.id, null, url, context.signal, deadline);
    if (waitForNavigation) tab = await waitComplete(tab.id, context.signal, deadline, !strictComplete);
    return tabInfo(tab);
  }
  const tab = await resolveTab(args, context);
  context.tabId = tab.id;
  const willNavigate = open || ["Chrome.Refresh", "Chrome.GoBack", "Chrome.GoForward"].includes(code);
  const before = willNavigate && waitForNavigation ? await chrome.webNavigation.getFrame({tabId: tab.id, frameId: 0}).catch(() => null) : null;
  switch (code) {
    case "Chrome.OpenUrl":
    case "Chrome.JumpUrl":
      await chrome.tabs.update(tab.id, {url});
      break;
    case "Chrome.Refresh": await chrome.tabs.reload(tab.id); break;
    case "Chrome.GoBack": await chrome.tabs.goBack(tab.id); break;
    case "Chrome.GoForward": await chrome.tabs.goForward(tab.id); break;
    case "Chrome.SwitchTab":
    case "Chrome.SwitchToTab":
      if (context.lastTabId !== tab.id) context.frameId = undefined;
      await chrome.tabs.update(tab.id, {active: true});
      await chrome.windows.update(tab.windowId, {focused: true});
      return tabInfo(await chrome.tabs.get(tab.id));
    case "Chrome.CloseTab":
    case "Chrome.CloseTabByTitle":
      await chrome.tabs.remove(tab.id);
      context.tabId = undefined;
      context.frameId = undefined;
      return {closed: tab.id};
  }
  selectedFrames.delete(tab.id);
  context.frameId = undefined;
  const committed = waitForNavigation ? await waitForCommit(tab.id, before, url, context.signal, deadline) : await chrome.tabs.get(tab.id);
  return tabInfo(waitForNavigation ? await waitComplete(tab.id, context.signal, deadline, !strictComplete) : committed);
}
const navigationCodes = new Set(["Chrome.ListTabs", "Chrome.CloseExtensionsTabs", "Chrome.OpenUrl", "Chrome.JumpUrl",
  "Chrome.Refresh", "Chrome.GoBack", "Chrome.GoForward", "Chrome.SwitchTab", "Chrome.SwitchToTab", "Chrome.CloseTab", "Chrome.CloseTabByTitle"]);

async function sendDOM(tabId, frameId, code, data, signal, documentId) {
  const operation = crypto.randomUUID();
  const options = documentId ? {documentId} : {frameId};
  const target = documentId ? {tabId, documentIds: [documentId]} : {tabId, frameIds: [frameId]};
  const message = {endpoint: "ghosthand.dom", code, operation, data};
  const send = () => chrome.tabs.sendMessage(tabId, message, options);
  function abort() {
    if (code === "Chrome.Wait") chrome.tabs.sendMessage(tabId,
      {endpoint: message.endpoint, code: "Browser.cancel", operation}, options).catch(() => {});
  }
  signal.throwIfAborted();
  signal.addEventListener("abort", abort, {once: true});
  try {
    if (code === "Chrome.ScreenShotToBase64") {
      await chrome.scripting.executeScript({target, files: ["js/html2canvas.js"]});
    }
    let response;
    try { response = await send(); }
    catch (error) {
      if (!/Receiving end does not exist/.test(error.message)) throw error;
      await chrome.scripting.executeScript({target, files: ["content-script.js"], injectImmediately: true});
      signal.throwIfAborted();
      response = await send();
    }
    signal.throwIfAborted();
    if (!response?.ok) throw new Error(response?.error || "网页没有返回操作结果");
    return response.data;
  } finally {
    signal.removeEventListener("abort", abort);
  }
}

async function allFrames(tabId) {
  return (await chrome.webNavigation.getAllFrames({tabId}) ?? [])
    .filter(frame => !frame.documentLifecycle || frame.documentLifecycle === "active");
}

async function inspectTab(tab, context, args) {
  const frames = await allFrames(tab.id);
  if (context.frameId !== undefined) {
    const frame = frames.find(frame => frame.frameId === context.frameId);
    return {...await sendDOM(tab.id, context.frameId, "Chrome.InspectDOM", args, context.signal, frame?.documentId),
      tab_id: tab.id, frame_id: context.frameId};
  }

  let remaining = args.max_elements ?? 150;
  const skipped = [];
  let top;
  let truncated = false;
  let size = 0;
  const observed = [];
  for (const frame of frames.sort((a, b) => a.frameId - b.frameId)) {
    context.signal.throwIfAborted();
    if (!remaining) {
      truncated = true;
      break;
    }
    try {
      const {elements, ...data} = await sendDOM(tab.id, frame.frameId, "Chrome.InspectDOM",
        {...args, max_elements: remaining, include_text: frame.frameId === 0 && args.include_text !== false},
        context.signal, frame.documentId);
      if (frame.frameId === 0) top = data;
      truncated ||= data.truncated;
      for (const element of elements) {
        const entry = {...element, frame_id: frame.frameId};
        size += encoder.encode(JSON.stringify(entry)).length + 1;
        if (size > 170000) {
          truncated = true;
          remaining = 0;
          break;
        }
        observed.push(entry);
        remaining--;
      }
    } catch (error) {
      if (frame.frameId === 0) throw error;
      skipped.push(frame.frameId);
    }
  }
  context.signal.throwIfAborted();
  if (!top) throw new Error("无法读取标签页主文档");
  return {...top, tab_id: tab.id, frame_id: 0, elements: observed, truncated,
    frames: frames.slice(0, 32).map(frame => ({frame_id: frame.frameId, parent_frame_id: frame.parentFrameId, url: frame.url.slice(0, 160)})),
    skipped_frames: skipped};
}

async function switchFrame(tabId, parentId, args, context) {
  const frames = await allFrames(tabId);
  const children = new Set(frames.filter(frame => frame.parentFrameId === parentId).map(frame => frame.frameId));
  if (!children.size) throw new Error("当前文档下没有 iframe");
  const id = crypto.randomUUID();
  let resolve;
  const accepted = new Promise(set => { resolve = set; });
  probes.set(id, {tabId, children, resolve});
  const timer = new AbortController();
  const signal = AbortSignal.any([timer.signal, context.signal]);
  try {
    await sendDOM(tabId, parentId, "Chrome.SwitchToFrame", {...args, probe: id}, signal);
    const frameId = await Promise.race([accepted, sleep(Math.min(args.timeout_ms ?? 2000, 5000), signal)
      .then(() => { throw new Error("所选 iframe 未建立 DOM 通道"); })]);
    selectedFrames.set(tabId, frameId);
    context.frameId = frameId;
    return {tab_id: tabId, frame_id: frameId};
  } finally {
    probes.delete(id);
    timer.abort();
  }
}

async function cookies(code, tab, frameId, args) {
  const frame = frameId ? await chrome.webNavigation.getFrame({tabId: tab.id, frameId}) : null;
  const page = checkedUrl(frame ? frame.url : tab.url, false);
  const url = checkedUrl(args.url ?? page.href, false);
  if (page.origin !== url.origin) throw new Error("Cookie 操作必须与目标文档同源");
  const stores = await chrome.cookies.getAllCookieStores();
  const storeId = stores.find(store => store.tabIds.includes(tab.id))?.id;
  if (!storeId) throw new Error("找不到标签页的 Cookie 存储");
  const details = {url: url.href, storeId};
  if (code === "Chrome.GetCookie") {
    if (args.name) return chrome.cookies.get({...details, name: args.name});
    const entries = await chrome.cookies.getAll(details);
    return entries.map(({value, ...cookie}) => cookie.httpOnly ? cookie : {...cookie, value});
  }
  return chrome.cookies.set({...details, name: args.name, value: String(args.value), path: args.path,
    httpOnly: args.http_only, secure: args.secure, sameSite: args.same_site, expirationDate: args.expiration_date});
}

function pageScript(script, args, expression) {
  const body = expression ? "return (\n" + script + "\n);" : script;
  return '(async (args) => {\n"use strict"; try {\n' +
    'const value = await (async () => {\n' + body + '\n})();\n' +
    'return {ok:true,value: value === undefined ? null : JSON.parse(JSON.stringify(value))};\n' +
    '} catch (error) { return {ok:false,error:String(error)}; }\n' +
    '})(JSON.parse(' + JSON.stringify(JSON.stringify(args)) + '))';
}

async function evaluate(tabId, frameId, data) {
  const target = {tabId, frameIds: [frameId]};
  let userScripts;
  try { userScripts = chrome.userScripts; }
  catch { /* The browser has disabled user script injection for this extension. */ }
  if (userScripts?.execute) {
    let injected;
    for (const expression of [true, false]) {
      try {
        [injected] = await userScripts.execute({target, world: "MAIN", injectImmediately: true,
          js: [{code: pageScript(data.script, data.args ?? [], expression)}]});
      } catch (error) {
        if (expression && /syntax|unexpected token|unexpected identifier/i.test(error.message)) continue;
        throw error;
      }
      if (injected?.result) break;
      if (injected?.error && !/syntax|unexpected token|unexpected identifier/i.test(String(injected.error))) {
        throw new Error(String(injected.error));
      }
    }
    if (!injected?.result?.ok) throw new Error(injected?.result?.error ?? injected?.error ?? "页面 JavaScript 未返回结果");
    return injected.result.value;
  }
  const [result] = await chrome.scripting.executeScript({
    target, world: "MAIN", injectImmediately: true,
    args: [data.script, data.args ?? []],
    func: async (script, args) => {
      try {
        const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
        let fn;
        try { fn = new AsyncFunction("args", '"use strict"; return (\n' + script + '\n);'); }
        catch (error) {
          if (!(error instanceof SyntaxError)) throw error;
          fn = new AsyncFunction("args", '"use strict";\n' + script);
        }
        const value = await fn(args);
        return {ok: true, value: value === undefined ? null : JSON.parse(JSON.stringify(value))};
      } catch (error) {
        return {ok: false, error: error.name === "EvalError" ? "页面内容安全策略禁止动态 JavaScript" : String(error)};
      }
    }
  });
  if (!result?.result?.ok) throw new Error(result?.result?.error ?? "页面 JavaScript 未返回结果");
  return result.result.value;
}

const batchCodes = {
  open: "Chrome.OpenUrl", navigate: "Chrome.OpenUrl", activate: "Chrome.SwitchTab", close: "Chrome.CloseTab", reload: "Chrome.Refresh",
  back: "Chrome.GoBack", forward: "Chrome.GoForward", inspect: "Chrome.InspectDOM", list_frames: "Chrome.ListFrames",
  click: "Chrome.Click", click_link: "Chrome.ClickByLinkText", fill: "Chrome.SetValue", select: "Chrome.SetSelect",
  check: "Chrome.setRadioCheckBox", hover: "Chrome.Hover", focus: "Chrome.Focus",
  scroll: "Chrome.ScrollPage", scroll_to_view: "Chrome.ScrollToView",
  wait: "Chrome.Wait", switch_frame: "Chrome.SwitchToFrame", switch_top_frame: "Chrome.SwitchToTopFrame",
  get_text: "Chrome.GetText", get_html: "Chrome.GetinnerHTML", get_attr: "Chrome.GetAttributes", get_similar: "Chrome.GetSimilar",
  exists: "Chrome.Exists", get_value: "Chrome.GetValue", get_select: "Chrome.GetSelect",
  get_options: "Chrome.GetSelectOptions", get_checked: "Chrome.getRadioCheckBoxStatus",
  get_rect: "Chrome.GetRectangleByXpath", get_parent: "Chrome.GetParentElement",
  get_children: "Chrome.GetChildElement", get_siblings: "Chrome.GetSiblingElement",
  get_title: "Chrome.GetTitle", get_url: "Chrome.GetUrl", get_source: "Chrome.GetViewSource",
  cookie_get: "Chrome.GetCookie", cookie_set: "Chrome.SetCookie"
};

async function batch(steps, context) {
  const results = [];
  let size = 0;
  context.inBatch = true;
  for (const [index, step] of steps.entries()) {
    const code = step.action === "scroll" && [step.gh_id, step.xpath, step.selector, step.text].some(Boolean) ?
      "Chrome.ScrollToView" : batchCodes[step.action];
    try {
      context.signal.throwIfAborted();
      if (!code) throw new Error("不支持的批处理动作");
      if (step.tab_id !== undefined && context.tabId !== step.tab_id) context.frameId = undefined;
      context.tabId = step.tab_id ?? context.tabId;
      context.frameId = step.frame_id ?? context.frameId;
      const result = {index, action: step.action, data: await execute(code, step, context)};
      size += encoder.encode(JSON.stringify(result)).length + 1;
      if (size > maxResultBytes - 20000) throw new Error("批处理结果超出大小上限");
      results.push(result);
    } catch (error) {
      throw Object.assign(new Error("第 " + (index + 1) + " 步 " + step.action + " 失败：" + (error.message || error)),
        {batch: {failed_index: index, action: step.action, results}});
    }
  }
  return {results};
}

async function execute(code, args, context) {
  context.signal.throwIfAborted();
  if (code === "Browser.ping") return {connected: true, browser, extension_id: chrome.runtime.id};
  if (code === "Chrome.UninstallSelf") {
    uninstalling = true;
    // Chrome tears down the worker before this API's successful promise can resolve.
    setTimeout(() => {
      chrome.management.uninstallSelf({showConfirmDialog: false}).catch(error => {
        uninstalling = false;
        console.warn("GhostHand extension removal:", error.message);
      });
    }, 100);
    return {removing: true};
  }
  if (navigationCodes.has(code)) {
    context.lastTabId = context.tabId;
    return navigation(code, args, context);
  }
  if (code === "Chrome.BatchExecute") return batch(args.steps, context);

  const tab = await resolveTab({}, context);
  context.tabId = tab.id;
  const frame = context.frameId ?? selectedFrames.get(tab.id) ?? 0;
  switch (code) {
    case "Chrome.ListFrames": return (await allFrames(tab.id)).map(item =>
      ({frame_id: item.frameId, parent_frame_id: item.parentFrameId, url: item.url, document_id: item.documentId}));
    case "Chrome.InspectDOM": return inspectTab(tab, context, args);
    case "Chrome.SwitchToFrame": return switchFrame(tab.id, frame, args, context);
    case "Chrome.SwitchToTopFrame":
      selectedFrames.delete(tab.id);
      context.frameId = 0;
      return {tab_id: tab.id, frame_id: 0};
    case "Chrome.EvaluateJS":
      checkedUrl(tab.url);
      return evaluate(tab.id, frame, args);
    case "Chrome.GetCookie":
    case "Chrome.SetCookie": return cookies(code, tab, frame, args);
    case "Chrome.CaptureVisibleTab":
      checkedUrl(tab.url);
      // captureVisibleTab reads the active tab, so verify the target after capture as well.
      if (!tab.active) await chrome.tabs.update(tab.id, {active: true});
      {
        const jpeg = await chrome.tabs.captureVisibleTab(tab.windowId, {format: "jpeg", quality: 82});
        const [active] = await chrome.tabs.query({windowId: tab.windowId, active: true});
        if (active?.id !== tab.id) throw new Error("截图期间活动标签页发生切换");
        return {jpeg};
      }
    case "Chrome.Wait": {
      const until = Date.now() + (args.timeout_ms ?? 10000);
      while (true) {
        try { return await sendDOM(tab.id, frame, code, {...args, timeout_ms: Math.max(1, until - Date.now())}, context.signal); }
        catch (error) {
          if (Date.now() >= until || !/message (channel|port)|Receiving end|No (document|frame)|Cannot access contents of url/i.test(error.message)) throw error;
          await sleep(100, context.signal);
        }
      }
    }
    default: {
      const data = await sendDOM(tab.id, frame, code, args, context.signal);
      if (args.wait_complete) {
        const deadline = Date.now() + (args.timeout_ms ?? 10000);
        await sleep(100, context.signal);
        await waitComplete(tab.id, context.signal, deadline);
      }
      return data;
    }
  }
}

void ensureAlarm();
connect();
