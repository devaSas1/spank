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
        updateView() // Update mirroring live while dragging
    }

    override func mouseUp(with event: NSEvent) {
        dragging = false
        updateView()
    }
}

let app = NSApplication.shared
let window = OverlayWindow()
let imageView = NSImageView(frame: window.contentView!.bounds)
imageView.imageScaling = .scaleProportionallyUpOrDown
imageView.wantsLayer = true
window.contentView?.addSubview(imageView)

// Thought Label (Glassmorphic Bubble)
let thoughtLabel = NSTextField(frame: NSRect(x: 10, y: 155, width: 180, height: 40))
thoughtLabel.isEditable = false
thoughtLabel.isBordered = false
thoughtLabel.isBezeled = false
thoughtLabel.drawsBackground = true
thoughtLabel.backgroundColor = NSColor.black.withAlphaComponent(0.6)
thoughtLabel.textColor = .white
thoughtLabel.font = NSFont.systemFont(ofSize: 11, weight: .medium)
thoughtLabel.alignment = .center
thoughtLabel.lineBreakMode = .byWordWrapping
thoughtLabel.wantsLayer = true
thoughtLabel.layer?.cornerRadius = 8
thoughtLabel.isHidden = true
window.contentView?.addSubview(thoughtLabel)

window.makeKeyAndOrderFront(nil)

let args = CommandLine.arguments
let assetDir = args.count > 1 ? args[1] : "./sus_assets/visual"

var currentLevel = 1
var currentIdle = 1
var lastSlapTime = Date()
var lastIdleChange = Date()
var currentContextTag = "mode_unknown"
var currentContextMood = ""
var currentContextThought = ""
var lastThoughtUpdate = Date()

func updateView() {
    let screenRect = NSScreen.main?.frame ?? NSScreen.screens.first?.frame ?? .zero
    let windowCenterX = window.frame.midX
    let shouldMirror = windowCenterX > screenRect.midX
    
    DispatchQueue.main.async {
        // Ensure the image view fills the window
        imageView.frame = window.contentView?.bounds ?? .zero
        
        let transform: CATransform3D
        if shouldMirror {
            // Flip around the center of the image view
            let centerX = imageView.bounds.midX
            var t = CATransform3DIdentity
            t = CATransform3DTranslate(t, centerX, 0, 0)
            t = CATransform3DScale(t, -1, 1, 1)
            t = CATransform3DTranslate(t, -centerX, 0, 0)
            transform = t
        } else {
            transform = CATransform3DIdentity
        }
        
        imageView.layer?.transform = transform
        
        // Update thought bubble
        if currentContextThought.isEmpty || currentContextTag == "mode_unknown" {
            thoughtLabel.isHidden = true
        } else {
            thoughtLabel.stringValue = currentContextThought
            thoughtLabel.isHidden = false
            lastThoughtUpdate = Date() // Reset the 'timer' anchor
            
            // Subtle fade-in
            thoughtLabel.alphaValue = 0
            NSAnimationContext.runAnimationGroup { context in
                context.duration = 0.5
                thoughtLabel.animator().alphaValue = 0.8 // Reaching a slightly transparent peak
            }
        }
        
        if window.dragging {
            loadGrabbedImage()
        } else {
            if !loadContextImage(tag: currentContextTag) {
                loadImage(level: currentLevel)
            }
        }
    }
}

func loadContextImage(tag: String) -> Bool {
    let cleanTag = tag.trimmingCharacters(in: .whitespacesAndNewlines)
    if cleanTag.isEmpty || cleanTag == "mode_unknown" {
        return false
    }

    // --- COIN FLIP LOGIC ---
    let variant = Bool.random() ? "1" : "2"
    let variantTag = "\(cleanTag)_\(variant)"
    
    // Check for the randomized variant first (e.g., mode_focus_1.png)
    let variantGif = "\(assetDir)/\(variantTag).gif"
    let variantPng = "\(assetDir)/\(variantTag).png"
    
    // Also check for the base tag (e.g., mode_focus.png) as a fallback
    let baseGif = "\(assetDir)/\(cleanTag).gif"
    let basePng = "\(assetDir)/\(cleanTag).png"
    
    var candidatePaths: [String] = [variantGif, variantPng, baseGif, basePng]

    for base in contextFallbackBaseNames(tag: cleanTag) {
        candidatePaths.append("\(assetDir)/\(base).gif")
        candidatePaths.append("\(assetDir)/\(base).png")
    }

    for p in candidatePaths where FileManager.default.fileExists(atPath: p) {
        if let img = NSImage(contentsOfFile: p) {
            imageView.image = img
            return true
        }
    }
    return false
}

func contextFallbackBaseNames(tag: String) -> [String] {
    switch tag {
    case "mode_focus":
        return ["2", "1", "0_1"]
    case "mode_chill":
        return ["0_2", "0_1", "1"]
    case "mode_game":
        return ["3", "2", "1"]
    case "mode_music":
        return ["0_3", "0_2", "1"]
    case "mode_shame":
        return ["4", "2", "1"]
    default:
        return []
    }
}

func loadGrabbedImage() {
    let path = "\(assetDir)/grabbed.png"
    if let img = NSImage(contentsOfFile: path) {
        imageView.image = img
    } else {
        // Fallback to current level if grabbed photo is missing
        loadImage(level: currentLevel)
    }
}

func loadImage(level: Int) {
    var baseName = "\(level)"
    if level == 0 {
        baseName = "0_\(currentIdle)"
    }
    
    let gifPath = "\(assetDir)/\(baseName).gif"
    let pngPath = "\(assetDir)/\(baseName).png"
    
    var finalPath = pngPath
    if FileManager.default.fileExists(atPath: gifPath) {
        finalPath = gifPath
    } else if level == 0 && !FileManager.default.fileExists(atPath: pngPath) {
        currentIdle = 1
        finalPath = "\(assetDir)/0_1.png"
    }

    if let img = NSImage(contentsOfFile: finalPath) {
        imageView.image = img
    } else {
        imageView.image = nil
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
    
    // Auto-fade thought bubble after 10 seconds
    if !thoughtLabel.isHidden && now.timeIntervalSince(lastThoughtUpdate) > 10.0 {
        NSAnimationContext.runAnimationGroup { context in
            context.duration = 1.0
            thoughtLabel.animator().alphaValue = 0
        } completionHandler: {
            if Date().timeIntervalSince(lastThoughtUpdate) >= 11.0 { // Verification to avoid race
                thoughtLabel.isHidden = true
            }
        }
    }
}

DispatchQueue.global().async {
    while let line = readLine() {
        let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed == "quit" {
            exit(0)
        }
        if let data = trimmed.data(using: .utf8),
           let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           let kind = obj["type"] as? String {
            if kind == "slap" {
                if let level = obj["level"] as? Int {
                    DispatchQueue.main.async {
                        currentLevel = level
                        lastSlapTime = Date()
                        updateView()
                    }
                    continue
                }
            }
            if kind == "context" {
                let tag = (obj["tag"] as? String) ?? "mode_unknown"
                let mood = (obj["mood"] as? String) ?? ""
                let thought = (obj["thought"] as? String) ?? ""
                DispatchQueue.main.async {
                    currentContextTag = tag
                    currentContextMood = mood
                    currentContextThought = thought
                    updateView()
                }
                continue
            }
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
