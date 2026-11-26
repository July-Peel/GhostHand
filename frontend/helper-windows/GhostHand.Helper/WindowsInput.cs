using System.ComponentModel;
using System.Runtime.InteropServices;

namespace GhostHand.Helper;

internal static class WindowsInput
{
    private const uint MouseType = 0;
    private const uint KeyboardType = 1;
    private const uint KeyUp = 0x0002;
    private const uint ExtendedKey = 0x0001;
    private const uint Unicode = 0x0004;
    private const uint LeftDown = 0x0002;
    private const uint LeftUp = 0x0004;
    private const uint RightDown = 0x0008;
    private const uint RightUp = 0x0010;
    private const uint MiddleDown = 0x0020;
    private const uint MiddleUp = 0x0040;
    private static readonly Dictionary<string, ushort> VirtualKeys = new(StringComparer.OrdinalIgnoreCase)
    {
        ["Ctrl"] = 0x11, ["Control"] = 0x11, ["Alt"] = 0x12, ["Shift"] = 0x10,
        ["Enter"] = 0x0D, ["Escape"] = 0x1B, ["Esc"] = 0x1B, ["Tab"] = 0x09, ["Space"] = 0x20,
        ["Back"] = 0x08, ["Backspace"] = 0x08, ["Delete"] = 0x2E, ["Insert"] = 0x2D,
        ["Home"] = 0x24, ["End"] = 0x23, ["PageUp"] = 0x21, ["PageDown"] = 0x22,
        ["Left"] = 0x25, ["Up"] = 0x26, ["Right"] = 0x27, ["Down"] = 0x28,
        ["Win"] = 0x5B, ["LWin"] = 0x5B, ["RWin"] = 0x5C, ["Apps"] = 0x5D, ["ContextMenu"] = 0x5D,
        ["PrintScreen"] = 0x2C, ["ScrollLock"] = 0x91, ["Pause"] = 0x13, ["CapsLock"] = 0x14, ["NumLock"] = 0x90,
        ["BrowserBack"] = 0xA6, ["BrowserForward"] = 0xA7, ["BrowserRefresh"] = 0xA8,
        ["VolumeMute"] = 0xAD, ["VolumeDown"] = 0xAE, ["VolumeUp"] = 0xAF,
        [";"] = 0xBA, ["="] = 0xBB, [","] = 0xBC, ["-"] = 0xBD, ["."] = 0xBE, ["/"] = 0xBF,
        ["`"] = 0xC0, ["["] = 0xDB, ["\\"] = 0xDC, ["]"] = 0xDD, ["'"] = 0xDE,
    };

    public static System.Windows.Rect VirtualScreen => new(GetSystemMetrics(76), GetSystemMetrics(77),
        GetSystemMetrics(78), GetSystemMetrics(79));

    public static void Click(int x, int y, string button, bool twice, bool hoverOnly, string[]? modifiers, CancellationToken token)
    {
        var modCodes = (modifiers ?? []).Select(KeyCode).Distinct().ToList();
        if (modCodes.Count > 0)
        {
            Emit(modCodes.Select(k => Key(k, 0, KeyFlags(k))).ToArray(), token);
        }
        try
        {
            if (hoverOnly)
            {
                Emit([MoveTo(x, y)], token);
                return;
            }
            var (down, up) = button switch
            {
                "right" => (RightDown, RightUp),
                "middle" => (MiddleDown, MiddleUp),
                _ => (LeftDown, LeftUp),
            };
            var input = new List<Input> { MoveTo(x, y), Mouse(down), Mouse(up) };
            if (twice)
            {
                input.Add(Mouse(down));
                input.Add(Mouse(up));
            }
            Emit(input.ToArray(), token);
        }
        finally
        {
            if (modCodes.Count > 0)
            {
                var release = new List<Input>(modCodes.Count);
                for (var i = modCodes.Count - 1; i >= 0; i--)
                {
                    release.Add(Key(modCodes[i], 0, KeyFlags(modCodes[i]) | KeyUp));
                }
                Emit(release.ToArray(), CancellationToken.None);
            }
        }
    }

    public static void WakeMouse()
    {
        SendInput(1, [Mouse(0x0001, 0, 0, 0)], Marshal.SizeOf<Input>());
    }

    public static void Scroll(int x, int y, string direction, int distance, CancellationToken token)
    {
        var horizontal = direction is "left" or "right";
        var delta = Math.Clamp(distance == 0 ? 3 : (distance + 119) / 120, 1, 10) * 120;
        if (direction is "down" or "left")
        {
            delta = -delta;
        }
        Emit([MoveTo(x, y), Mouse(horizontal ? 0x1000u : 0x0800u, unchecked((uint)delta))], token);
    }

    public static void Drag(int x, int y, int endX, int endY, CancellationToken token)
    {
        Emit([MoveTo(x, y), Mouse(LeftDown)], token);
        try
        {
            for (var step = 1; step <= 8; step++)
            {
                Emit([MoveTo(x + (endX - x) * step / 8, y + (endY - y) * step / 8)], token);
                if (token.WaitHandle.WaitOne(12))
                {
                    token.ThrowIfCancellationRequested();
                }
            }
        }
        finally
        {
            Emit([Mouse(LeftUp)], CancellationToken.None);
        }
    }

    public static void Keys(string[] keys, CancellationToken token, int repeat = 1)
    {
        const string shifted = ":+<_>?~{|}\"!@#$%^&*()";
        const string baseKeys = ";=,-./`[\\]'1234567890";
        var needsShift = false;
        var codes = keys.Select(name =>
        {
            var index = name.Length == 1 ? shifted.IndexOf(name[0]) : -1;
            needsShift |= index >= 0;
            return KeyCode(index >= 0 ? baseKeys[index].ToString() : name);
        }).ToList();
        if (needsShift && !codes.Contains((ushort)0x10))
        {
            codes.Insert(0, 0x10);
        }
        var count = Math.Clamp(repeat, 1, 50);
        for (var r = 0; r < count; r++)
        {
            var input = new List<Input>(codes.Count * 2);
            input.AddRange(codes.Select(key => Key(key, 0, KeyFlags(key))));
            for (var i = codes.Count - 1; i >= 0; i--)
            {
                input.Add(Key(codes[i], 0, KeyFlags(codes[i]) | KeyUp));
            }
            Emit(input.ToArray(), token);
        }
    }

    public static void Text(string text, CancellationToken token)
    {
        var input = new List<Input>(text.Length * 2);
        foreach (var character in text.Replace("\r\n", "\n", StringComparison.Ordinal))
        {
            if (character == '\n')
            {
                input.Add(Key(0x0D, 0, 0));
                input.Add(Key(0x0D, 0, KeyUp));
            }
            else
            {
                input.Add(Key(0, character, Unicode));
                input.Add(Key(0, character, Unicode | KeyUp));
            }
        }
        if (input.Count > 0)
        {
            Emit(input.ToArray(), token);
        }
    }

    private static ushort KeyCode(string name)
    {
        if (VirtualKeys.TryGetValue(name, out var code))
        {
            return code;
        }
        if (name.Length == 1 && char.IsAsciiLetterOrDigit(name[0]))
        {
            return char.ToUpperInvariant(name[0]);
        }
        if (name.Length is >= 2 and <= 3 && (name[0] is 'F' or 'f') &&
            int.TryParse(name.AsSpan(1), out var function) && function is >= 1 and <= 24)
        {
            return (ushort)(0x6F + function);
        }
        throw new CaptureException("E_KEY_UNSUPPORTED");
    }

    private static uint KeyFlags(ushort code) => code is >= 0x21 and <= 0x2E or >= 0x5B and <= 0x5D or 0x90 or >= 0xA6 and <= 0xAF
        ? ExtendedKey : 0;

    private static void Emit(Input[] input, CancellationToken token)
    {
        token.ThrowIfCancellationRequested();
        var sent = SendInput((uint)input.Length, input, Marshal.SizeOf<Input>());
        if (sent != input.Length)
        {
            var error = Marshal.GetLastWin32Error();
            if (error == 0)
            {
                throw new CaptureException(DesktopCapture.IsDefaultDesktop() ? "E_INPUT_BLOCKED" : "E_DESKTOP_LOCKED");
            }
            throw new Win32Exception(error);
        }
    }

    private static Input MoveTo(int x, int y)
    {
        var left = GetSystemMetrics(76);
        var top = GetSystemMetrics(77);
        var width = Math.Max(2, GetSystemMetrics(78));
        var height = Math.Max(2, GetSystemMetrics(79));
        return Mouse(0xC001, 0, (int)((long)(x - left) * 65535 / (width - 1)), (int)((long)(y - top) * 65535 / (height - 1)));
    }

    private static Input Mouse(uint flags, uint data = 0, int x = 0, int y = 0) =>
        new() { Type = MouseType, Union = new() { Mouse = new() { X = x, Y = y, Data = data, Flags = flags } } };

    private static Input Key(ushort virtualKey, ushort scan, uint flags) =>
        new() { Type = KeyboardType, Union = new() { Keyboard = new() { VirtualKey = virtualKey, Scan = scan, Flags = flags } } };

    [StructLayout(LayoutKind.Sequential)]
    private struct Input
    {
        public uint Type;
        public InputUnion Union;
    }

    [StructLayout(LayoutKind.Explicit)]
    private struct InputUnion
    {
        [FieldOffset(0)] public MouseInput Mouse;
        [FieldOffset(0)] public KeyboardInput Keyboard;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct MouseInput
    {
        public int X, Y;
        public uint Data, Flags, Time;
        public UIntPtr Extra;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct KeyboardInput
    {
        public ushort VirtualKey, Scan;
        public uint Flags, Time;
        public UIntPtr Extra;
    }

    [DllImport("user32.dll", SetLastError = true)]
    private static extern uint SendInput(uint count, [In] Input[] input, int size);

    [DllImport("user32.dll")]
    private static extern int GetSystemMetrics(int index);
}
