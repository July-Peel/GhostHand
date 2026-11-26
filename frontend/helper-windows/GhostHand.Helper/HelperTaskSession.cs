using System.IO;
using System.Runtime.InteropServices;
using System.Text.Json;

namespace GhostHand.Helper;

internal sealed class HelperTaskSession(StreamReader reader, StreamWriter writer)
{
    private readonly SemaphoreSlim writeGate = new(1);
    private readonly CancellationTokenSource lifetime = new();
    private TaskCompletionSource? heartbeatReply;
    private int heartbeatId = 1;
    private CancellationTokenSource? captureCancel;
    private Task? capture;
    private string? captureTask;
    private readonly WindowsTools tools = new();

    public async Task RunAsync()
    {
        using var marquee = new TaskMarquee(SendControlAsync);
        var read = ReadAsync(marquee);
        var heartbeat = HeartbeatAsync();
        try
        {
            await await Task.WhenAny(read, heartbeat);
        }
        finally
        {
            lifetime.Cancel();
            await IgnorePipeFailureAsync(read);
            await IgnorePipeFailureAsync(heartbeat);
            if (capture is Task pending)
            {
                await IgnorePipeFailureAsync(pending);
            }
            writeGate.Dispose();
            lifetime.Dispose();
        }
    }

    private async Task ReadAsync(TaskMarquee marquee)
    {
        while (!lifetime.IsCancellationRequested)
        {
            var line = await reader.ReadLineAsync(lifetime.Token) ??
                throw new EndOfStreamException("GhostHand Core closed the pipe.");
            using var json = JsonDocument.Parse(line);
            var frame = json.RootElement;
            if (frame.GetProperty("jsonrpc").GetString() != "2.0")
            {
                throw new IOException("Invalid GhostHand IPC version.");
            }
            if (frame.TryGetProperty("method", out var method))
            {
                var data = frame.GetProperty("params");
                var task = data.GetProperty("task_id").GetString() ?? throw new IOException("Missing task ID.");
                switch (method.GetString())
                {
                    case "task.overlay":
                        marquee.Update(task, data.GetProperty("visible").GetBoolean(),
                            data.TryGetProperty("state", out var state) ? state.GetString() ?? "" : "",
                            data.TryGetProperty("title", out var title) ? title.GetString() ?? "" : "设备任务",
                            data.TryGetProperty("step_index", out var step) || data.TryGetProperty("step", out step) ? step.GetInt32() : 0,
                            data.TryGetProperty("step_total", out var total) ? total.GetInt32() : 0,
                            data.TryGetProperty("step_name", out var name) ? name.GetString() ?? "" : "");
                        break;
                    case "screen.capture":
                    case "mcp.call":
                    case "browser.ext_install":
                    case "browser.ext_remove_uia":
                    case "browser.ext_close_uia":
                        var id = frame.GetProperty("id").GetInt32();
                        if (capture is { IsCompleted: false })
                        {
                            await SendAsync(new { jsonrpc = "2.0", id, result = new { error = "E_BUSY" } }, lifetime.Token);
                            break;
                        }
                        captureTask = task;
                        var methodName = method.GetString()!;
                        capture = ExecuteAsync(marquee, id, task, methodName == "screen.capture" ? null : data.Clone(), methodName);
                        break;
                    case "screen.cancel":
                        if (task == captureTask)
                        {
                            captureCancel?.Cancel();
                        }
                        break;
                    default:
                        throw new IOException("Unknown GhostHand IPC request.");
                }
            }
            else if (frame.GetProperty("id").GetInt32() == heartbeatId && frame.GetProperty("result").GetBoolean())
            {
                heartbeatReply?.TrySetResult();
            }
            else
            {
                throw new IOException("Invalid GhostHand heartbeat reply.");
            }
        }
    }

    private async Task HeartbeatAsync()
    {
        using var ticker = new PeriodicTimer(TimeSpan.FromSeconds(10));
        while (await ticker.WaitForNextTickAsync(lifetime.Token))
        {
            heartbeatReply = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var id = ++heartbeatId;
            using var deadline = CancellationTokenSource.CreateLinkedTokenSource(lifetime.Token);
            deadline.CancelAfter(TimeSpan.FromSeconds(5));
            await SendAsync(new { jsonrpc = "2.0", id, method = "helper.heartbeat", @params = new { } }, deadline.Token);
            await heartbeatReply.Task.WaitAsync(deadline.Token);
        }
    }

    private async Task ExecuteAsync(TaskMarquee marquee, int id, string task, JsonElement? tool, string method)
    {
        using var cancel = CancellationTokenSource.CreateLinkedTokenSource(lifetime.Token);
        captureCancel = cancel;
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancel.Token);
        object result;
        var internalBrowserCall = method.StartsWith("browser.", StringComparison.Ordinal);
        var toolName = internalBrowserCall ? method : tool?.GetProperty("tool_name").GetString();
        timeout.CancelAfter(TimeSpan.FromSeconds(toolName == "ui.wait_for" ? 34 : toolName is "terminal.open" or "app.launch" ? 25 : 20));
        var screenshot = tool is null || toolName == "screen.observe";
        try
        {
            if (screenshot)
            {
                await marquee.PrepareCaptureAsync(task, timeout.Token);
            }
            if (tool is JsonElement call && toolName is not null)
            {
                var approved = call.TryGetProperty("approved", out var approval) && approval.GetBoolean();
                var window = call.TryGetProperty("window_id", out var expected) ? expected.GetString() ?? "" : "";
                var arguments = internalBrowserCall ? call : call.GetProperty("arguments");
                result = await Task.Run(() => tools.CallAsync(task, toolName, arguments, window, approved, timeout.Token), timeout.Token);
            }
            else
            {
                var jpeg = await Task.Run(() => DesktopCapture.CaptureAsync(timeout.Token), timeout.Token);
                result = new { jpeg };
            }
        }
        catch (OperationCanceledException)
        {
            result = new { error = cancel.IsCancellationRequested ? "E_CANCELLED" : "E_TIMEOUT" };
        }
        catch (CaptureException error)
        {
            result = new { error = error.Message };
        }
        catch (Exception error) when (error is UnauthorizedAccessException ||
            error is COMException { HResult: unchecked((int)0x80070005) } ||
            error is System.ComponentModel.Win32Exception { NativeErrorCode: 5 })
        {
            result = new { error = DesktopCapture.IsDefaultDesktop() ? "E_ACCESS_DENIED: " + error.Message : "E_DESKTOP_LOCKED" };
        }
        catch (Exception)
        {
            result = new { error = "E_INTERNAL" };
        }
        finally
        {
            if (screenshot)
            {
                marquee.FinishCapture(task);
            }
            captureCancel = null;
        }

        try
        {
            await SendAsync(new { jsonrpc = "2.0", id, result }, lifetime.Token);
        }
        catch (Exception error) when (error is OperationCanceledException or IOException or ObjectDisposedException)
        {
            lifetime.Cancel();
        }
    }

    private async Task SendControlAsync(string id, string action)
    {
        try
        {
            await SendAsync(new { jsonrpc = "2.0", method = "task.control", @params = new { task_id = id, action } }, lifetime.Token);
        }
        catch (Exception error) when (error is OperationCanceledException or IOException or ObjectDisposedException)
        {
            if (!lifetime.IsCancellationRequested)
            {
                lifetime.Cancel();
            }
        }
    }

    private async Task SendAsync(object frame, CancellationToken token)
    {
        await writeGate.WaitAsync(token);
        try
        {
            await writer.WriteLineAsync(JsonSerializer.Serialize(frame).AsMemory(), token);
        }
        finally
        {
            writeGate.Release();
        }
    }

    private static async Task IgnorePipeFailureAsync(Task task)
    {
        try
        {
            await task;
        }
        catch (Exception error) when (error is OperationCanceledException or IOException or JsonException)
        {
            // The original failure is propagated by RunAsync after the other loops stop.
        }
    }
}
