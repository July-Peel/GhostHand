package proto

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const (
	ToolBrowserManage  = "browser.ext_manage"
	ToolBrowserTabs    = "browser.tabs"
	ToolBrowserInspect = "browser.inspect"
	ToolBrowserAct     = "browser.act"
	ToolBrowserEval    = "browser.eval"
)

const browserFrameSchema = `"tab_id":{"type":"integer","minimum":1},"frame_id":{"type":"integer","minimum":0}`
const browserTargetSchema = `"browser":{"type":"string","enum":["chrome","edge"]},` + browserFrameSchema
const browserInspectSchema = `"filter_text":{"type":"string","maxLength":200},"include_text":{"type":"boolean"},"max_elements":{"type":"integer","minimum":1,"maximum":400}`
const browserNavigationSchema = `"url":{"type":"string","maxLength":4096},"title_contains":{"type":"string","maxLength":200},"title_regex":{"type":"string","maxLength":120},"url_contains":{"type":"string","maxLength":200},"index":{"type":"integer","minimum":0},"new_tab":{"type":"boolean"},"wait_complete":{"type":"boolean"}`
const browserTimeoutSchema = `"timeout_ms":{"type":"integer","minimum":1,"maximum":30000}`
const browserActionSchema = `"gh_id":{"type":"integer","minimum":1},"xpath":{"type":"string","maxLength":2048},"selector":{"type":"string","maxLength":2048},"text":{"type":"string","maxLength":1000},"value":{"type":["string","number","boolean","array"],"items":{"type":"string"},"maxItems":100},"clear_first":{"type":"boolean"},"submit":{"type":"boolean"},"state":{"type":"string","enum":["appear","disappear","text_contains","complete"]},"text_contains":{"type":"string","maxLength":1000},"all":{"type":"boolean"},"attribute":{"type":"string","maxLength":100},"attributes":{"type":"array","items":{"type":"string","maxLength":100},"maxItems":32},"direction":{"type":"string","enum":["up","down","left","right"]},"distance":{"type":"integer","minimum":0,"maximum":50000},"pages":{"type":"integer","minimum":1,"maximum":20},"name":{"type":"string","maxLength":256},"path":{"type":"string","maxLength":2048},"http_only":{"type":"boolean"},"secure":{"type":"boolean"},"same_site":{"type":"string","enum":["no_restriction","lax","strict","unspecified"]},"expiration_date":{"type":"number","minimum":0}`

const browserTabActions = "list open navigate activate close reload back forward"
const browserDOMActions = "click click_link fill select check hover focus scroll scroll_to_view wait switch_frame switch_top_frame list_frames exists get_text get_html get_attr get_similar get_value get_select get_options get_checked get_rect get_parent get_children get_siblings get_title get_url get_source cookie_get cookie_set"
const browserStepActions = browserDOMActions + " open navigate activate close reload back forward inspect"
const browserToolActions = browserStepActions + " screenshot_element screenshot_tab batch"

const browserActionProperties = browserFrameSchema + "," + browserInspectSchema + "," + browserNavigationSchema + "," + browserTimeoutSchema + "," + browserActionSchema

func browserActionEnum(actions string) string {
	return `"action":{"type":"string","enum":["` + strings.ReplaceAll(actions, " ", `","`) + `"]}`
}

var browserTools = []MCPTool{
	{ToolBrowserManage, "查询连接状态、按需加载或卸载 GhostHand Browser Bridge 扩展。", toolSchema(
		browserActionEnum("status install uninstall")+`,"browser":{"type":"string","enum":["chrome","edge"]}`, `"action"`), "interactive"},
	{ToolBrowserTabs, "列出、打开、激活、关闭浏览器标签页，或刷新、前进、后退。", toolSchema(
		browserActionEnum(browserTabActions)+","+browserTargetSchema+","+browserNavigationSchema+","+browserTimeoutSchema, `"action"`), "interactive"},
	{ToolBrowserInspect, "读取网页标题、正文摘要和可交互 DOM 元素。元素编号在所属 frame_id 内有效。", toolSchema(
		browserTargetSchema+","+browserInspectSchema, ""), "read_only"},
	{ToolBrowserAct, "执行 DOM 动作或按序执行 steps。批处理中可以跨页导航、等待元素和提取结果。截图用单步动作。", toolSchema(
		browserActionEnum(browserToolActions)+`,"browser":{"type":"string","enum":["chrome","edge"]},`+browserActionProperties+`,"steps":{"type":"array","minItems":1,"maxItems":32,"items":`+
			string(toolSchema(browserActionEnum(browserStepActions)+","+browserActionProperties, `"action"`))+"}", `"action"`), "interactive"},
	{ToolBrowserEval, "在目标标签页或 iframe 的页面上下文执行 JavaScript，args 数组可在脚本中引用。", toolSchema(
		browserTargetSchema+`,"script":{"type":"string","minLength":1,"maxLength":64000},"args":{"type":"array","maxItems":64}`, `"script"`), "interactive"},
}

type BrowserArgs struct {
	Action         string            `json:"action"`
	Browser        string            `json:"browser"`
	TabID          *int              `json:"tab_id"`
	FrameID        *int              `json:"frame_id"`
	Index          *int              `json:"index"`
	URL            string            `json:"url"`
	TitleContains  string            `json:"title_contains"`
	TitleRegex     string            `json:"title_regex"`
	URLContains    string            `json:"url_contains"`
	NewTab         bool              `json:"new_tab"`
	WaitComplete   bool              `json:"wait_complete"`
	MaxElements    *int              `json:"max_elements"`
	FilterText     string            `json:"filter_text"`
	IncludeText    *bool             `json:"include_text"`
	GHID           *int              `json:"gh_id"`
	XPath          string            `json:"xpath"`
	Selector       string            `json:"selector"`
	Text           string            `json:"text"`
	Value          json.RawMessage   `json:"value"`
	ClearFirst     bool              `json:"clear_first"`
	Submit         bool              `json:"submit"`
	TimeoutMS      *int              `json:"timeout_ms"`
	State          string            `json:"state"`
	TextContains   string            `json:"text_contains"`
	All            bool              `json:"all"`
	Attribute      string            `json:"attribute"`
	Attributes     []string          `json:"attributes"`
	Direction      string            `json:"direction"`
	Distance       *int              `json:"distance"`
	Pages          *int              `json:"pages"`
	Name           string            `json:"name"`
	Path           string            `json:"path"`
	HTTPOnly       bool              `json:"http_only"`
	Secure         bool              `json:"secure"`
	SameSite       string            `json:"same_site"`
	ExpirationDate *float64          `json:"expiration_date"`
	Script         string            `json:"script"`
	Args           []json.RawMessage `json:"args"`
	Steps          []json.RawMessage `json:"steps"`
}

const browserCommonKeys = "browser tab_id frame_id"
const browserInspectKeys = "filter_text include_text max_elements"
const browserNavKeys = "url title_contains title_regex url_contains index new_tab wait_complete timeout_ms"
const browserDOMKeys = "gh_id xpath selector text value clear_first submit state text_contains all attribute attributes direction distance pages name path http_only secure same_site expiration_date"

func IsBrowserTool(name string) bool {
	switch name {
	case ToolBrowserManage, ToolBrowserTabs, ToolBrowserInspect, ToolBrowserAct, ToolBrowserEval:
		return true
	}
	return false
}

func (a BrowserArgs) HasLocator() bool {
	return a.GHID != nil || a.XPath != "" || a.Selector != "" || a.Text != ""
}

func ValidateBrowserTool(name string, data json.RawMessage) (BrowserArgs, error) {
	return validateBrowserTool(name, data, false)
}

func validateBrowserTool(name string, data json.RawMessage, step bool) (BrowserArgs, error) {
	var args BrowserArgs
	var fields map[string]json.RawMessage
	if len(data) > MaxToolArgsSize || json.Unmarshal(data, &fields) != nil || fields == nil || json.Unmarshal(data, &args) != nil {
		return args, ErrToolArguments
	}
	keys, actions := browserCommonKeys, ""
	switch name {
	case ToolBrowserManage:
		keys, actions = "action browser", "status install uninstall"
	case ToolBrowserTabs:
		keys, actions = keys+" action "+browserNavKeys, browserTabActions
	case ToolBrowserInspect:
		keys += " " + browserInspectKeys
	case ToolBrowserEval:
		keys += " script args"
	case ToolBrowserAct:
		keys += " action " + browserInspectKeys + " " + browserNavKeys + " " + browserDOMKeys
		actions = browserStepActions
		if step {
			keys = strings.TrimPrefix(keys, "browser ")
		} else {
			keys += " steps"
			actions = browserToolActions
		}
	default:
		return args, ErrToolArguments
	}
	if actions != "" && !browserChoice(args.Action, actions) {
		return args, ErrToolArguments
	}
	for key, value := range fields {
		if !browserChoice(key, keys) || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return args, ErrToolArguments
		}
	}
	if !browserOptional(args.Browser, "chrome edge") ||
		!browserBound(args.TabID, 1, 1<<31-1) || !browserBound(args.FrameID, 0, 1<<31-1) ||
		!browserBound(args.Index, 0, 1<<31-1) || !browserBound(args.GHID, 1, 1<<31-1) ||
		!browserBound(args.MaxElements, 1, 400) || !browserBound(args.TimeoutMS, 1, 30000) ||
		!browserBound(args.Distance, 0, 50000) || !browserBound(args.Pages, 1, 20) ||
		len(args.TitleContains) > 200 || len(args.TitleRegex) > 120 || len(args.URLContains) > 200 ||
		len(args.XPath) > 2048 || len(args.Selector) > 2048 || len(args.Text) > 1000 || len(args.TextContains) > 1000 ||
		len(args.FilterText) > 200 || len(args.Name) > 256 || len(args.Path) > 2048 ||
		len(args.Attribute) > 100 || len(args.Attributes) > 32 ||
		slices.ContainsFunc(args.Attributes, func(s string) bool { return s == "" || len(s) > 100 }) ||
		!browserOptional(args.State, "appear disappear text_contains complete") ||
		!browserOptional(args.Direction, "up down left right") ||
		!browserOptional(args.SameSite, "no_restriction lax strict unspecified") ||
		(args.ExpirationDate != nil && *args.ExpirationDate < 0) {
		return args, ErrToolArguments
	}
	if args.TitleRegex != "" {
		if _, err := regexp.Compile(args.TitleRegex); err != nil {
			return args, ErrToolArguments
		}
	}
	if args.URL != "" && !validBrowserURL(args.URL) {
		return args, ErrToolArguments
	}
	if name == ToolBrowserEval && (strings.TrimSpace(args.Script) == "" || len(args.Script) > 64000 || len(args.Args) > 64) {
		return args, ErrToolArguments
	}
	if args.Action != "batch" && fields["steps"] != nil {
		return args, ErrToolArguments
	}
	valid := true
	switch args.Action {
	case "open", "navigate":
		valid = args.URL != ""
	case "batch":
		valid = len(args.Steps) > 0 && len(args.Steps) <= 32
		for _, raw := range args.Steps {
			if _, err := validateBrowserTool(name, raw, true); err != nil {
				valid = false
				break
			}
		}
	case "wait":
		valid = args.State == "complete" || (args.HasLocator() && (args.State != "text_contains" || args.TextContains != ""))
	case "fill":
		valid = args.HasLocator() && browserScalar(args.Value)
	case "check":
		valid = args.HasLocator() && (len(args.Value) == 0 || string(args.Value) == "true" || string(args.Value) == "false")
	case "select":
		valid = args.HasLocator() && (args.Index != nil || browserSelection(args.Value))
	case "cookie_set":
		valid = strings.TrimSpace(args.Name) != "" && browserScalar(args.Value)
	case "click", "click_link", "hover", "focus", "scroll_to_view", "switch_frame", "exists", "get_text", "get_html", "get_attr",
		"get_similar", "get_value", "get_select", "get_options", "get_checked", "get_rect", "get_parent", "get_children",
		"get_siblings", "screenshot_element":
		valid = args.HasLocator()
	}
	if !valid {
		return args, ErrToolArguments
	}
	return args, nil
}

func browserChoice(value, choices string) bool {
	return value != "" && strings.IndexByte(value, ' ') < 0 && strings.Contains(" "+choices+" ", " "+value+" ")
}

func browserOptional(value, choices string) bool {
	return value == "" || browserChoice(value, choices)
}

func browserBound(value *int, minimum, maximum int) bool {
	return value == nil || (*value >= minimum && *value <= maximum)
}

func browserString(data json.RawMessage) bool {
	var value string
	return len(data) > 0 && json.Unmarshal(data, &value) == nil
}

func browserScalar(data json.RawMessage) bool {
	if browserString(data) {
		return true
	}
	if string(data) == "true" || string(data) == "false" {
		return true
	}
	var number float64
	return len(data) > 0 && json.Unmarshal(data, &number) == nil
}

func browserSelection(data json.RawMessage) bool {
	if browserString(data) {
		return true
	}
	var number float64
	if json.Unmarshal(data, &number) == nil {
		return true
	}
	var values []string
	return json.Unmarshal(data, &values) == nil && len(values) > 0 && len(values) <= 100
}

func validBrowserURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return u.Hostname() != ""
	case "file":
		return u.Host == "" && strings.HasPrefix(u.Path, "/")
	}
	return false
}
