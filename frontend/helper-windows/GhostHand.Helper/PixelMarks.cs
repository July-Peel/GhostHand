using System.Globalization;
using System.Windows;
using Windows.Graphics.Imaging;
using Windows.Storage.Streams;

namespace GhostHand.Helper;

internal readonly record struct ScreenRegion(int X, int Y, int Width, int Height)
{
    public static ScreenRegion FromRect(Rect bounds) => bounds.IsEmpty
        ? new(0, 0, 0, 0)
        : new((int)bounds.Left, (int)bounds.Top, (int)bounds.Width, (int)bounds.Height);

    public int[] ToArray() => [X, Y, Width, Height];

    public ScreenRegion Clip(int width, int height)
    {
        var left = Math.Clamp(X, 0, width);
        var top = Math.Clamp(Y, 0, height);
        return new ScreenRegion(left, top, Math.Clamp(X + Width, left, width) - left, Math.Clamp(Y + Height, top, height) - top);
    }
}

internal readonly record struct ScreenMark(int ID, ScreenRegion Region);

internal static class PixelMarks
{
    private static readonly string[] Digits =
    [
        "111101101101111", "010110010010111", "111001111100111", "111001111001111", "101101111001001",
        "111100111001111", "111100111101111", "111001001001001", "111101111101111", "111101111001111"
    ];

    public static void Paint(SoftwareBitmap bitmap, IReadOnlyList<ScreenMark> marks, CancellationToken token)
    {
        var width = bitmap.PixelWidth;
        var height = bitmap.PixelHeight;
        var bytes = new byte[checked(width * height * 4)];
        var buffer = new Windows.Storage.Streams.Buffer((uint)bytes.Length) { Length = (uint)bytes.Length };
        bitmap.CopyToBuffer(buffer);
        using (var reader = DataReader.FromBuffer(buffer))
        {
            reader.ReadBytes(bytes);
        }

        foreach (var mark in marks)
        {
            token.ThrowIfCancellationRequested();
            var bounds = mark.Region.Clip(width, height);
            if (bounds.Width > 0 && bounds.Height > 0)
            {
                Outline(bounds);
            }
        }
        foreach (var mark in marks)
        {
            token.ThrowIfCancellationRequested();
            var text = mark.ID.ToString(CultureInfo.InvariantCulture);
            var labelWidth = 9 * text.Length + 8;
            var left = Math.Clamp(mark.Region.X, 0, Math.Max(0, width - labelWidth));
            var top = Math.Clamp(mark.Region.Y - 17, 0, Math.Max(0, height - 17));
            Fill(new ScreenRegion(left, top, labelWidth, 17), false);
            for (var i = 0; i < text.Length; i++)
            {
                var glyph = Digits[text[i] - '0'];
                for (var j = 0; j < glyph.Length; j++)
                {
                    if (glyph[j] == '1')
                    {
                        Fill(new ScreenRegion(left + 4 + 9 * i + 2 * (j % 3), top + 3 + 2 * (j / 3), 2, 2), true);
                    }
                }
            }
        }
        using var writer = new DataWriter();
        writer.WriteBytes(bytes);
        bitmap.CopyFromBuffer(writer.DetachBuffer());

        void Outline(ScreenRegion bounds)
        {
            Fill(bounds with { Height = 2 }, false);
            Fill(bounds with { Y = bounds.Y + bounds.Height - 2, Height = 2 }, false);
            Fill(bounds with { Width = 2 }, false);
            Fill(bounds with { X = bounds.X + bounds.Width - 2, Width = 2 }, false);
        }

        void Fill(ScreenRegion rectangle, bool white)
        {
            var box = rectangle.Clip(width, height);
            for (var y = box.Y; y < box.Y + box.Height; y++)
            for (var x = box.X; x < box.X + box.Width; x++)
            {
                var offset = (y * width + x) * 4;
                bytes[offset] = white ? (byte)255 : (byte)229;
                bytes[offset + 1] = white ? (byte)255 : (byte)70;
                bytes[offset + 2] = white ? (byte)255 : (byte)79;
                bytes[offset + 3] = 255;
            }
        }
    }
}
