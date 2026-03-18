import Cocoa

class OverlayWindow: NSWindow {
    var dragging = false
    var lastLocation: NSPoint = .zero

    init() {
        let screenRect = NSScreen.main?.frame ?? NSRect(x: 0, y: 0, width: 800, height: 600)
        let w = 200.0, h = 200.0
        let rect = NSRect(x: screenRect.width - w - 50, y: 50, width: w, height: h)
        super.init(contentRect: rect, styleMask: .borderless, backing: .buffered, defer: false)
        self.isOpaque = false
        self.backgroundColor = .clear
        self.level = .floating 
        self.hasShadow = false
        self.ignoresMouseEvents = false
        self.collectionBehavior = [.canJoinAllSpaces, .stationary, .ignoresCycle]
        self.isMovableByWindowBackground = false
    }

    override var canBecomeKey: Bool { return true }

    // Estimated peak coordinate in the 200x200 window
    let anchorX: CGFloat = 100 
    let anchorY: CGFloat = 175

    override func mouseDown(with event: NSEvent) {
        dragging = true
        updateView()
        
        // Instant snap: move the window so the 'peak' is exactly where the mouse is
        let screenMouse = NSEvent.mouseLocation
        let newOrigin = NSPoint(x: screenMouse.x - anchorX, y: screenMouse.y - anchorY)
        self.setFrameOrigin(newOrigin)
        
        // Lock the 'lastLocation' to the anchor so dragging is smooth from that point
        lastLocation = NSPoint(x: anchorX, y: anchorY)
        
        print("grabbed")
        fflush(stdout)
    }

    override func mouseDragged(with event: NSEvent) {
        let newOrigin = self.frame.origin
        // Move the window based on how much the mouse has moved from the anchor
        let newX = newOrigin.x + (event.locationInWindow.x - lastLocation.x)
        let newY = newOrigin.y + (event.locationInWindow.y - lastLocation.y)
        self.setFrameOrigin(NSPoint(x: newX, y: newY))
        updateView() // Update mirroring while dragging
    }

    override func mouseUp(with event: NSEvent) {
        dragging = false
        updateView()
    }
}

let app = NSApplication.shared
let window = OverlayWindow()
let contentView = window.contentView!
let imageView = NSImageView(frame: contentView.bounds)
imageView.imageScaling = .scaleProportionallyUpOrDown
imageView.wantsLayer = true
if let layer = imageView.layer {
    layer.anchorPoint = CGPoint(x: 0.5, y: 0.5)
    layer.frame = contentView.bounds
}
contentView.addSubview(imageView)
window.makeKeyAndOrderFront(nil)

let args = CommandLine.arguments
let assetDir = args.count > 1 ? args[1] : "./sus_assets/visual"

var currentLevel = 1
var currentIdle = 1
var lastSlapTime = Date()
var lastIdleChange = Date()

func updateView() {
    let screen = window.screen ?? NSScreen.main
    let screenRect = screen?.frame ?? NSRect.zero
    let windowCenterX = window.frame.midX
    let shouldMirror = windowCenterX > screenRect.midX
    
    DispatchQueue.main.async {
        if let layer = imageView.layer {
            // Use a stable transform: reset to identity, then flip across the midline if needed
            if shouldMirror {
                // Translation-Scale-Translation to flip across the 100px vertical center
                var transform = CATransform3DMakeTranslation(200, 0, 0)
                transform = CATransform3DScale(transform, -1, 1, 1)
                layer.transform = transform
            } else {
                layer.transform = CATransform3DIdentity
            }
        }
    }

    if window.dragging {
        loadGrabbedImage()
    } else {
        loadImage(level: currentLevel)
    }
}

func loadGrabbedImage() {
    let path = "\(assetDir)/grabbed.png"
    if let img = NSImage(contentsOfFile: path) {
        DispatchQueue.main.async {
            imageView.image = img
        }
    } else {
        // Fallback to current level if grabbed photo is missing
        loadImage(level: currentLevel)
    }
}

func loadImage(level: Int) {
    var path = "\(assetDir)/\(level).png"
    if level == 0 {
        path = "\(assetDir)/0_\(currentIdle).png"
    }
    
    // Fallback if that specific idle doesn't exist
    if level == 0 && !FileManager.default.fileExists(atPath: path) {
        currentIdle = 1
        path = "\(assetDir)/0_\(currentIdle).png"
    }

    if let img = NSImage(contentsOfFile: path) {
        DispatchQueue.main.async {
            imageView.image = img
        }
    } else {
        // As a final fallback if 0_1 doesn't exist either, just clear
        DispatchQueue.main.async { imageView.image = nil }
    }
}

func nextIdle() {
    currentIdle = Int.random(in: 1...5)
    lastIdleChange = Date()
    updateView()
}

// Start with Nani
updateView()

// Setup the timer for fading out interactions and swapping idle expressions
Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { _ in
    let now = Date()
    let sinceSlap = now.timeIntervalSince(lastSlapTime)
    
    if currentLevel > 0 {
        if sinceSlap > 30.0 {
            currentLevel = 0
            nextIdle()
        } else if sinceSlap > 15.0 && currentLevel > 2 {
            currentLevel = 2
            updateView()
        }
    } else {
        if now.timeIntervalSince(lastIdleChange) > 300.0 {
            nextIdle()
        }
    }
}

DispatchQueue.global().async {
    while let line = readLine() {
        let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed == "quit" {
            exit(0)
        }
        if let level = Int(trimmed) {
            DispatchQueue.main.async {
                currentLevel = level
                lastSlapTime = Date()
                updateView()
            }
        }
    }
    exit(0)
}

app.run()
