// sfync-notifier: a minimal UNUserNotificationCenter helper.
//
// It exists only because UNUserNotificationCenter (and the legacy
// NSUserNotification/osascript path it replaced) requires the calling
// process to belong to a real app bundle with a stable bundle identifier —
// a bare binary, or `osascript` invoked from a script, has no identity for
// macOS to grant (and remember) notification permission against. Packaging
// this as its own tiny .app gives sfync's daemon a real, persistent
// notification identity.
//
// Usage: sfync-notifier <title> <message> [urgency]
//   urgency: "critical" plays a sound; anything else (default) is silent.

import Foundation
import UserNotifications

func fail(_ message: String, code: Int32) -> Never {
    FileHandle.standardError.write((message + "\n").data(using: .utf8)!)
    exit(code)
}

let arguments = CommandLine.arguments
guard arguments.count >= 3 else {
    fail("usage: sfync-notifier <title> <message> [urgency]", code: 64)
}

let title = arguments[1]
let message = arguments[2]
let urgency = arguments.count >= 4 ? arguments[3] : "normal"

let center = UNUserNotificationCenter.current()

// Resolve (and, on first run, request) authorization before attempting to
// post. Both steps are async, so block on a semaphore with a generous
// timeout rather than exiting before the completion handler fires.
let authSemaphore = DispatchSemaphore(value: 0)
var authorized = false

center.getNotificationSettings { settings in
    switch settings.authorizationStatus {
    case .authorized, .provisional:
        authorized = true
        authSemaphore.signal()
    case .notDetermined:
        center.requestAuthorization(options: [.alert, .sound]) { granted, _ in
            authorized = granted
            authSemaphore.signal()
        }
    default:
        // .denied or unknown future case — nothing we can do without the
        // user re-enabling it in System Settings.
        authSemaphore.signal()
    }
}

// Generous timeout: the .notDetermined branch above waits on a system
// permission dialog, which needs a human to click it.
if authSemaphore.wait(timeout: .now() + 60) == .timedOut {
    fail("sfync-notifier: timed out waiting for notification authorization", code: 1)
}
guard authorized else {
    fail("sfync-notifier: notifications not authorized (check System Settings > Notifications > sfync)", code: 1)
}

let content = UNMutableNotificationContent()
content.title = title
content.body = message
content.sound = urgency == "critical" ? .default : nil

let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)

let postSemaphore = DispatchSemaphore(value: 0)
var postError: Error?
center.add(request) { error in
    postError = error
    postSemaphore.signal()
}
_ = postSemaphore.wait(timeout: .now() + 5)

if let postError {
    fail("sfync-notifier: \(postError.localizedDescription)", code: 1)
}
exit(0)
