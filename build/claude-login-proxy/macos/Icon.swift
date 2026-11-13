import AppKit

let output = URL(fileURLWithPath: CommandLine.arguments[1])
try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
for size in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let pixels = size * scale
        let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)
        let transform = AffineTransform(scale: CGFloat(pixels) / 1024)
        (transform as NSAffineTransform).concat()
        let rect = NSRect(x: 64, y: 64, width: 896, height: 896)
        let tile = NSBezierPath(roundedRect: rect, xRadius: 205, yRadius: 205)
        NSGradient(starting: NSColor(red: 0.22, green: 0.43, blue: 0.78, alpha: 1), ending: NSColor(red: 0.10, green: 0.23, blue: 0.48, alpha: 1))!.draw(in: tile, angle: -90)
        NSColor.white.setStroke()
        let route = NSBezierPath()
        route.lineWidth = 70; route.lineCapStyle = .round; route.lineJoinStyle = .round
        route.move(to: NSPoint(x: 315, y: 350)); route.line(to: NSPoint(x: 315, y: 510))
        route.curve(to: NSPoint(x: 485, y: 680), controlPoint1: NSPoint(x: 315, y: 625), controlPoint2: NSPoint(x: 370, y: 680))
        route.line(to: NSPoint(x: 704, y: 680)); route.stroke()
        let arrow = NSBezierPath(); arrow.lineWidth = 70; arrow.lineCapStyle = .round; arrow.lineJoinStyle = .round
        arrow.move(to: NSPoint(x: 603, y: 780)); arrow.line(to: NSPoint(x: 704, y: 680)); arrow.line(to: NSPoint(x: 603, y: 580)); arrow.stroke()
        NSColor.white.setFill()
        NSBezierPath(ovalIn: NSRect(x: 245, y: 250, width: 140, height: 140)).fill()
        NSColor.white.withAlphaComponent(0.5).setFill()
        NSBezierPath(ovalIn: NSRect(x: 634, y: 280, width: 100, height: 100)).fill()
        NSGraphicsContext.restoreGraphicsState()
        let name = "icon_\(size)x\(size)\(scale == 2 ? "@2x" : "").png"
        try bitmap.representation(using: .png, properties: [:])!.write(to: output.appendingPathComponent(name))
    }
}
