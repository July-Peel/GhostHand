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
		server := mcp.NewServer(&mcp.Implementation{Name: "ghosthand", Version: "0.4.5"}, nil)
		addDevicesTool(server, desk)
		for _, tool := range workbench.MCPSSHConnectionTools {
			addTool(server, desk, user, device, tool)
		}
		for _, tool := range proto.DeviceTools {
			if workbench.IsMCPToolAllowed(tool.Name) {
				addTool(server, desk, user, device, tool)
			}
		}
		for _, resource := range proto.DeviceResources {
			addResource(server, desk, user, "", resource)
		}
		if device != "" {
			if id, err := desk.MCPDeviceID(r.Context(), device); err == nil {
				_, resources, err := desk.MCPCapabilities(r.Context(), id)
				if err == nil {
					for _, resource := range resources {
						addResource(server, desk, user, id, resource)
					}
				}
			}
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
	tool := workbench.MCPDevicesTool
	server.AddTool(&mcp.Tool{
		Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]json.RawMessage
		raw := req.Params.Arguments
		if len(raw) > 0 && (json.Unmarshal(raw, &args) != nil || args == nil || len(args) != 0) {
			return toolError("devices_list 不接受参数。"), nil
		}
		devices, err := desk.MCPDevices(ctx)
		if err != nil {
			return toolError("暂时无法获取设备列表。"), nil
		}
		data, err := json.Marshal(map[string]any{"devices": devices})
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{StructuredContent: json.RawMessage(data), Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil
	})
}

func addTool(server *mcp.Server, desk *workbench.Service, user int64, device string, tool proto.MCPTool) {
	tool = workbench.AddressedMCPTool(tool, device == "")
	server.AddTool(&mcp.Tool{
		Name: strings.ReplaceAll(tool.Name, ".", "_"), Description: tool.Description, InputSchema: tool.InputSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool.RiskLevel == "read_only"},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		target, raw, err := workbench.MCPArguments(req.Params.Arguments, device)
		if err != nil {
			return toolError(err.Error()), nil
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
	case errors.Is(err, proto.ErrToolArguments), errors.Is(err, workbench.ErrMCPDeviceAmbiguous):
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
