using System.Runtime.InteropServices;
using System.Text;
using Windows.Foundation;
using Windows.Graphics.Capture;
using Windows.Graphics.DirectX;
using Windows.Graphics.DirectX.Direct3D11;
using Windows.Graphics.Imaging;
using Windows.Storage.Streams;
using WinRT;
using Rect = System.Windows.Rect;

namespace GhostHand.Helper;

internal static class DesktopCapture
{
    private const int PreferredBytes = 300 * 1024;
    private const int MaxBytes = 2 * 1024 * 1024;
    private static readonly Guid CaptureItemId = new("79C3F95B-31F7-4EC2-A464-632EF5D30760");

    public static async Task<byte[]> CaptureAsync(CancellationToken token,
        IReadOnlyList<ScreenMark>? marks = null, ScreenRegion? crop = null, IntPtr monitor = default)
    {
        if (!IsDefaultDesktop())
        {
            throw new CaptureException("E_DESKTOP_LOCKED");
        }
        if (!GraphicsCaptureSession.IsSupported())
        {
            throw new CaptureException("E_CAP_UNSUPPORTED");
        }

        if (monitor == IntPtr.Zero)
        {
            monitor = MonitorFromPoint(new PointNative(), 1); // MONITOR_DEFAULTTOPRIMARY
        }
        var interop = GraphicsCaptureItem.As<ICaptureItemInterop>();
        var itemId = CaptureItemId;
        Marshal.ThrowExceptionForHR(interop.CreateForMonitor(monitor, ref itemId, out var itemPtr));
        GraphicsCaptureItem item;
        try
        {
            item = MarshalInterface<GraphicsCaptureItem>.FromAbi(itemPtr);
        }
        finally
        {
            Marshal.Release(itemPtr);
        }

        using var device = CreateDevice();
        using var pool = Direct3D11CaptureFramePool.CreateFreeThreaded(
            device, DirectXPixelFormat.B8G8R8A8UIntNormalized, 1, item.Size);
        using var session = pool.CreateCaptureSession(item);
        session.IsCursorCaptureEnabled = false;

        var next = new TaskCompletionSource<Direct3D11CaptureFrame>(TaskCreationOptions.RunContinuationsAsynchronously);
        using var registration = token.Register(() => next.TrySetCanceled(token));
        void OnFrame(Direct3D11CaptureFramePool sender, object _)
        {
            try
            {
                var frame = sender.TryGetNextFrame();
                if (frame is not null && !next.TrySetResult(frame))
                {
                    frame.Dispose();
                }
            }
            catch (Exception error)
            {
                next.TrySetException(error);
            }
        }

        pool.FrameArrived += OnFrame;
        try
        {
            token.ThrowIfCancellationRequested();
            session.StartCapture();
            using var frame = await next.Task;
            using var bitmap = await SoftwareBitmap.CreateCopyFromSurfaceAsync(frame.Surface, BitmapAlphaMode.Ignore).AsTask(token);
            var width = Math.Min(bitmap.PixelWidth, frame.ContentSize.Width);
            var height = Math.Min(bitmap.PixelHeight, frame.ContentSize.Height);
            if (marks is { Count: > 0 })
            {
                PixelMarks.Paint(bitmap, marks, token);
            }
            var region = crop?.Clip(width, height) ?? new ScreenRegion(0, 0, width, height);
            if (region.Width <= 0 || region.Height <= 0)
            {
                throw new CaptureException("E_UI_POINT_OUTSIDE_WINDOW");
            }
            return await EncodeAsync(bitmap, region, token);
        }
        finally
        {
            pool.FrameArrived -= OnFrame;
        }
    }

    internal static CaptureDisplay SelectDisplay(int? index, IntPtr window)
    {
        var displays = Displays();
        if (index is int number)
        {
            if (number < 0 || number >= displays.Length)
            {
                throw new CaptureException("E_MONITOR_NOT_FOUND");
            }
            return displays[number];
        }
        var nearest = window == IntPtr.Zero ? IntPtr.Zero : MonitorFromWindow(window, 2);
        return displays.FirstOrDefault(display => display.Handle == nearest) ?? displays.FirstOrDefault()
            ?? throw new CaptureException("E_MONITOR_NOT_FOUND");
    }

    internal static CaptureDisplay[] Displays()
    {
        var monitors = new List<(IntPtr Handle, Rect Bounds, bool Primary)>();
        EnumDisplayMonitors(IntPtr.Zero, IntPtr.Zero, (monitor, _, _, _) =>
        {
            var info = new MonitorInfo { Size = (uint)Marshal.SizeOf<MonitorInfo>() };
            if (GetMonitorInfo(monitor, ref info))
            {
                var bounds = info.Monitor;
                monitors.Add((monitor, new Rect(bounds.Left, bounds.Top, bounds.Right - bounds.Left, bounds.Bottom - bounds.Top),
                    (info.Flags & 1) != 0));
            }
            return true;
        }, IntPtr.Zero);
        return monitors.OrderByDescending(display => display.Primary).ThenBy(display => display.Bounds.Left)
            .ThenBy(display => display.Bounds.Top).Select((display, index) =>
                new CaptureDisplay(display.Handle, display.Bounds, index, display.Primary)).ToArray();
    }

    private static async Task<byte[]> EncodeAsync(SoftwareBitmap bitmap, ScreenRegion region, CancellationToken token)
    {
        foreach (var (maxEdge, quality) in new[] { (1920, 0.84f), (1920, 0.72f), (1600, 0.68f), (1280, 0.60f) })
        {
            using var stream = new InMemoryRandomAccessStream();
            var options = new BitmapPropertySet { ["ImageQuality"] = new BitmapTypedValue(quality, PropertyType.Single) };
            var encoder = await BitmapEncoder.CreateAsync(BitmapEncoder.JpegEncoderId, stream, options).AsTask(token);
            encoder.SetSoftwareBitmap(bitmap);
            var largest = Math.Max(region.Width, region.Height);
            if (largest > maxEdge)
            {
                encoder.BitmapTransform.ScaledWidth = (uint)Math.Max(1, bitmap.PixelWidth * maxEdge / largest);
                encoder.BitmapTransform.ScaledHeight = (uint)Math.Max(1, bitmap.PixelHeight * maxEdge / largest);
                encoder.BitmapTransform.InterpolationMode = BitmapInterpolationMode.Fant;
            }
            var divisor = Math.Max(largest, maxEdge);
            var left = region.X * maxEdge / divisor;
            var top = region.Y * maxEdge / divisor;
            var right = (region.X + region.Width) * maxEdge / divisor;
            var bottom = (region.Y + region.Height) * maxEdge / divisor;
            encoder.BitmapTransform.Bounds = new BitmapBounds
            {
                X = (uint)left, Y = (uint)top,
                Width = (uint)Math.Max(1, right - left),
                Height = (uint)Math.Max(1, bottom - top)
            };
            await encoder.FlushAsync().AsTask(token);
            if (stream.Size <= PreferredBytes || (maxEdge == 1280 && stream.Size <= MaxBytes))
            {
                using var reader = new DataReader(stream.GetInputStreamAt(0));
                await reader.LoadAsync((uint)stream.Size).AsTask(token);
                var bytes = new byte[(int)stream.Size];
                reader.ReadBytes(bytes);
                return bytes;
            }
        }
        throw new CaptureException("E_INTERNAL");
    }

    private static IDirect3DDevice CreateDevice()
    {
        const uint bgraSupport = 0x20;
        const uint sdkVersion = 7;
        const int hardware = 1;
        const int warp = 5;
        var hr = D3D11CreateDevice(IntPtr.Zero, hardware, IntPtr.Zero, bgraSupport, IntPtr.Zero, 0,
            sdkVersion, out var device, IntPtr.Zero, IntPtr.Zero);
        if (hr < 0)
        {
            hr = D3D11CreateDevice(IntPtr.Zero, warp, IntPtr.Zero, bgraSupport, IntPtr.Zero, 0,
                sdkVersion, out device, IntPtr.Zero, IntPtr.Zero);
        }
        Marshal.ThrowExceptionForHR(hr);
        try
        {
            var dxgiId = new Guid("54EC77FA-1377-44E6-8C32-88FD5F44C84C");
            Marshal.ThrowExceptionForHR(Marshal.QueryInterface(device, ref dxgiId, out var dxgi));
            try
            {
                Marshal.ThrowExceptionForHR(CreateDirect3D11DeviceFromDXGIDevice(dxgi, out var wrapped));
                try
                {
                    return MarshalInterface<IDirect3DDevice>.FromAbi(wrapped);
                }
                finally
                {
                    Marshal.Release(wrapped);
                }
            }
            finally
            {
                Marshal.Release(dxgi);
            }
        }
        finally
        {
            Marshal.Release(device);
        }
    }

    internal static bool IsDefaultDesktop()
    {
        var desktop = OpenInputDesktop(0, false, 1); // DESKTOP_READOBJECTS
        if (desktop == IntPtr.Zero)
        {
            return false;
        }
        try
        {
            var name = new StringBuilder(64);
            return GetUserObjectInformation(desktop, 2, name, (uint)(name.Capacity * sizeof(char)), out _) &&
                name.ToString().Equals("Default", StringComparison.OrdinalIgnoreCase);
        }
        finally
        {
            CloseDesktop(desktop);
        }
    }

    [ComImport, Guid("3628E81B-3CAC-4C60-B7F4-23CE0E0C3356"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    private interface ICaptureItemInterop
    {
        [PreserveSig] int CreateForWindow(IntPtr window, ref Guid iid, out IntPtr item);
        [PreserveSig] int CreateForMonitor(IntPtr monitor, ref Guid iid, out IntPtr item);
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct PointNative
    {
        public int X;
        public int Y;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct NativeRect { public int Left, Top, Right, Bottom; }

    [StructLayout(LayoutKind.Sequential)]
    private struct MonitorInfo { public uint Size; public NativeRect Monitor, Work; public uint Flags; }

    private delegate bool MonitorCallback(IntPtr monitor, IntPtr device, IntPtr bounds, IntPtr data);

    [DllImport("user32.dll")]
    private static extern IntPtr MonitorFromPoint(PointNative point, uint flags);
    [DllImport("user32.dll")]
    private static extern IntPtr MonitorFromWindow(IntPtr window, uint flags);
    [DllImport("user32.dll")]
    private static extern bool EnumDisplayMonitors(IntPtr dc, IntPtr clip, MonitorCallback callback, IntPtr data);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern bool GetMonitorInfo(IntPtr monitor, ref MonitorInfo info);
    [DllImport("user32.dll")]
    private static extern IntPtr OpenInputDesktop(uint flags, bool inherit, uint access);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern bool GetUserObjectInformation(IntPtr desktop, int index, StringBuilder info, uint length, out uint required);
    [DllImport("user32.dll")]
    private static extern bool CloseDesktop(IntPtr desktop);
    [DllImport("d3d11.dll")]
    private static extern int D3D11CreateDevice(IntPtr adapter, int driverType, IntPtr software, uint flags,
        IntPtr featureLevels, uint featureLevelCount, uint sdkVersion, out IntPtr device,
        IntPtr selectedFeatureLevel, IntPtr immediateContext);
    [DllImport("d3d11.dll")]
    private static extern int CreateDirect3D11DeviceFromDXGIDevice(IntPtr dxgi, out IntPtr device);
}

internal sealed record CaptureDisplay(IntPtr Handle, Rect Bounds, int Index, bool Primary);

internal sealed class CaptureException(string code) : Exception(code)
{
}
