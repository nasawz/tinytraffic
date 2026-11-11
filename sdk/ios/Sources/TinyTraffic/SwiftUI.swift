#if canImport(SwiftUI)
import SwiftUI

public extension View {
    /// Records a screen view each time the view appears.
    ///
    ///     ClosetView().trackScreen("closet")
    func trackScreen(_ name: String) -> some View {
        onAppear { TinyTraffic.shared.screen(name) }
    }
}
#endif
