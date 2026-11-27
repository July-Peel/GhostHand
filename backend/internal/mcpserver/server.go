package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

type Gateway struct {
	HTTP http.Handler
	SSE  http.Handler
}

type userKey struct{}

func WithUser(r *http.Request, user int64) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey{}, user))
}

func New(desk *workbench.Service) Gateway {
	factory := func(r *http.Request) *mcp.Server {
		user, ok := r.Context().Value(userKey{}).(int64)
		if !ok {
			return nil
		}
		device := r.URL.Query().Get("device")
		tools, resources := proto.DeviceTools, proto.DeviceResources
		if device != "" {
			var err error
			tools, resources, err = desk.MCPCapabilities(r.Context(), device)
			if err != nil {
				return nil
			}
		}
		server := mcp.NewServer(&mcp.Implementation{Name: "ghosthand", Version: "0.4.5"}, nil)
		if device == "" {
			addDevicesTool(server, desk)
		}
		for _, tool := range tools {
			if workbench.IsMCPToolAllowed(tool.Name) {
				addTool(server, desk, user, device, tool)
			}
		}
		for _, resource := range resources {
			addResource(server, desk, user, device, resource)
		}
		return server
	}
	streamable := mcp.NewStreamableHTTPHandler(factory, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: proto.MaxToolArgsSize + (64 << 10),
	})
	legacy := mcp.NewSSEHandler(factory, &mcp.SSEOptions{MaxRequestBodyBytes: proto.MaxToolArgsSize + (64 << 10)})
	return Gateway{HTTP: streamable, SSE: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Accel-Buffering", "no")
		if r.Method == http.MethodGet {
			// The SDK emits the GET request's path as the per-session POST endpoint.
			r = r.Clone(r.Context())
			r.URL.Path, r.URL.RawPath = "/api/mcp/message", ""
		}
		legacy.ServeHTTP(w, r)
	})}
}

func addDevicesTool(server *mcp.Server, desk *workbench.Service) {
	server.AddTool(&mcp.Tool{
		Name: "devices_list", Description: "列出当前在线且支持 MCP 的 Windows 节点。",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]json.RawMessage
		raw := req.Params.Arguments
		if len(raw) > 0 && (json.Unmarshal(raw, &args) != nil || args == nil || len(args) != 0) {
			return toolError("devices_list 不接受参数。"), nil
		}
		devices, err := desk.MCPDevices(ctx)
		if err != nil {
			return toolError("暂时无法获取在线设备。"), nil
		}
		data, err := json.Marshal(map[string]any{"devices": devices})
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{StructuredContent: json.RawMessage(data), Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil
	})
}

func addTool(server *mcp.Server, desk *workbench.Service, user int64, device string, tool proto.MCPTool) {
	schema := tool.InputSchema
	if device == "" {
		var input struct {
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties bool                       `json:"additionalProperties"`
		}
		// InputSchema comes from the local protocol definitions.
		if err := json.Unmarshal(schema, &input); err != nil {
			panic(err)
		}
		if input.Properties == nil {
			input.Properties = make(map[string]json.RawMessage)
		}
		input.Properties["device_id"] = json.RawMessage(`{"type":"string","minLength":1,"maxLength":64}`)
		input.Required = append(input.Required, "device_id")
		schema, _ = json.Marshal(input)
	}
	server.AddTool(&mcp.Tool{
		Name: strings.ReplaceAll(tool.Name, ".", "_"), Description: tool.Description, InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool.RiskLevel == "read_only"},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		target := device
		raw := req.Params.Arguments
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		if target == "" {
			var args map[string]json.RawMessage
			if json.Unmarshal(raw, &args) != nil || json.Unmarshal(args["device_id"], &target) != nil || target == "" || len(target) > 64 {
				return toolError("请选择有效的 device_id。"), nil
			}
			delete(args, "device_id")
			raw, _ = json.Marshal(args)
		}
		result, image, err := read(ctx, desk, user, target, tool.Name, raw)
		if err != nil {
			failure := toolError(err.Error())
			if len(result) != 0 {
				failure.Content = append(failure.Content, &mcp.TextContent{Text: string(result)})
			}
			return failure, nil
		}
		content := []mcp.Content{&mcp.TextContent{Text: string(result)}}
		if len(image) > 0 {
			content = append(content, &mcp.ImageContent{Data: image, MIMEType: "image/jpeg"})
		}
		return &mcp.CallToolResult{StructuredContent: result, Content: content}, nil
	})
}

func addResource(server *mcp.Server, desk *workbench.Service, user int64, device string, resource proto.MCPResource) {
	const prefix = "ghosthand://device/"
	suffix := strings.TrimPrefix(resource.URI, prefix)
	tool := map[string]string{
		"status": proto.ToolStatus, "desktop": proto.ToolObserve,
		"windows": proto.ToolWindows, "processes": proto.ToolProcesses,
		"clipboard": proto.ToolClipboard,
	}[suffix]
	if tool == "" {
		return
	}
	handler := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		target := device
		if target == "" {
			target = strings.TrimSuffix(strings.TrimPrefix(req.Params.URI, prefix), "/"+suffix)
			if target == "" || len(target) > 64 || strings.ContainsAny(target, "/?#%") {
				return nil, errors.New("资源 URI 的设备 ID 无效。")
			}
			_, advertised, err := desk.MCPCapabilities(ctx, target)
			if err != nil {
				return nil, errors.New("设备资源不可用。")
			}
			found := false
			for _, entry := range advertised {
				found = found || entry.URI == resource.URI
			}
			if !found {
				return nil, errors.New("节点尚未提供此资源。")
			}
		}
		callArgs := json.RawMessage(`{}`)
		if tool == proto.ToolClipboard {
			callArgs = json.RawMessage(`{"action":"get"}`)
		}
		data, image, err := read(ctx, desk, user, target, tool, callArgs)
		if err != nil {
			return nil, err
		}
		content := &mcp.ResourceContents{URI: req.Params.URI, MIMEType: resource.MIMEType}
		if resource.MIMEType == "image/jpeg" {
			content.Blob = image
		} else if tool == proto.ToolClipboard {
			var clip struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(data, &clip) == nil {
				content.Text = clip.Text
			} else {
				content.Text = string(data)
			}
		} else {
			content.Text = string(data)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{content}}, nil
	}
	if device == "" {
		server.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: prefix + "{device_id}/" + suffix, Name: resource.Name, MIMEType: resource.MIMEType,
		}, handler)
	} else {
		server.AddResource(&mcp.Resource{URI: resource.URI, Name: resource.Name, MIMEType: resource.MIMEType}, handler)
	}
}

func read(ctx context.Context, desk *workbench.Service, user int64, device, tool string, args json.RawMessage) (json.RawMessage, []byte, error) {
	result, err := desk.CallMCP(ctx, user, device, tool, args)
	switch {
	case errors.Is(err, workbench.ErrDeviceNotFound):
		return nil, nil, errors.New("设备不存在或已撤销。")
	case errors.Is(err, workbench.ErrUnsupported):
		return nil, nil, errors.New("设备离线或尚未提供此工具。")
	case errors.Is(err, workbench.ErrTaskControl):
		return nil, nil, errors.New("此设备已有一项 MCP 诊断在执行。")
	case errors.Is(err, proto.ErrToolArguments):
		return nil, nil, err
	case errors.Is(err, context.DeadlineExceeded):
		return nil, nil, errors.New("等待设备诊断结果超时。")
	case errors.Is(err, context.Canceled):
		return nil, nil, errors.New("设备任务已取消。")
	case err != nil:
		return nil, nil, errors.New("设备诊断未完成。")
	}
	if result.Error != "" {
		return result.Data, nil, errors.New(result.Error)
	}
	var image []byte
	if tool == proto.ToolObserve || result.AttachmentID != "" {
		if result.AttachmentID != "" {
			image, err = desk.MCPImage(ctx, result.AttachmentID)
		}
		if err != nil || len(image) == 0 {
			return nil, nil, errors.New("设备截图不可用。")
		}
	}
	if proto.IsBrowserTool(tool) && (len(result.Data) == 0 || result.Data[0] != '{') {
		payload := result.Data
		if len(payload) == 0 {
			payload = json.RawMessage("null")
		}
		return append(append(json.RawMessage(`{"data":`), payload...), '}'), image, nil
	}
	return result.Data, image, nil
}

func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
}
