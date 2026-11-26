using Microsoft.Win32.SafeHandles;
using System.ComponentModel;
using System.IO;
using System.IO.Pipes;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json;

namespace GhostHand.Helper;

internal static class CoreConnection
{
    private const uint PipeReadWriteAccess = 0x0012019B;
    private const uint OpenExisting = 3;
    private const uint FileFlagOverlapped = 0x40000000;
    private const uint SecurityIdentification = 0x00010000;
    private const uint SecuritySqosPresent = 0x00100000;

    public static async Task RunAsync(string pipeName, string nonce)
    {
        // This access mask excludes FILE_CREATE_PIPE_INSTANCE.
        using var handle = CreateFile(
            $@"\\.\pipe\{pipeName}", PipeReadWriteAccess, 0, IntPtr.Zero, OpenExisting,
            FileFlagOverlapped | SecurityIdentification | SecuritySqosPresent, IntPtr.Zero);
        if (handle.IsInvalid)
        {
            throw new Win32Exception(Marshal.GetLastWin32Error());
        }

        await using var pipe = new NamedPipeClientStream(PipeDirection.InOut, true, true, handle);
        using var reader = new StreamReader(pipe, Encoding.UTF8, false, 1024, leaveOpen: true);
        await using var writer = new StreamWriter(pipe, new UTF8Encoding(false), 1024, leaveOpen: true)
        {
            AutoFlush = true
        };
        await SendAsync("helper.register", 1, new { nonce, tasks = true, mcp = true, browser_bridge = true }, reader, writer);
        await new HelperTaskSession(reader, writer).RunAsync();
    }

    private static async Task SendAsync(
        string method, int id, object parameters, StreamReader reader, StreamWriter writer)
    {
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        var request = JsonSerializer.Serialize(new { jsonrpc = "2.0", id, method, @params = parameters });
        await writer.WriteLineAsync(request.AsMemory(), deadline.Token);
        var line = await reader.ReadLineAsync(deadline.Token);
        if (line is null)
        {
            throw new EndOfStreamException("GhostHand Core closed the pipe.");
        }

        using var json = JsonDocument.Parse(line);
        var result = json.RootElement;
        if (result.ValueKind != JsonValueKind.Object ||
            !result.TryGetProperty("jsonrpc", out var protocol) || protocol.ValueKind != JsonValueKind.String ||
            protocol.GetString() != "2.0" ||
            !result.TryGetProperty("id", out var reply) || reply.ValueKind != JsonValueKind.Number ||
            !reply.TryGetInt32(out var replyId) || replyId != id ||
            !result.TryGetProperty("result", out var accepted) || accepted.ValueKind != JsonValueKind.True)
        {
            throw new IOException("GhostHand Core rejected the IPC request.");
        }
    }

    [DllImport("kernel32.dll", EntryPoint = "CreateFileW", SetLastError = true, CharSet = CharSet.Unicode, ExactSpelling = true)]
    private static extern SafePipeHandle CreateFile(
        string name, uint access, uint share, IntPtr security, uint creation, uint flags, IntPtr template);
}
