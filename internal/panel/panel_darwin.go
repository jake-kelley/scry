//go:build darwin

// This file is the project's third cgo boundary (alongside
// internal/fsevents and internal/hotkey — see hotkey_darwin.go's package
// doc for why §8 item 5's "one cgo boundary" grew by two, each kept
// small, single-purpose, and confined to its own file). It creates one
// borderless NSPanel hosting one WKWebView and does nothing else: no
// index, query, or IPC code runs on this side of the boundary, the same
// discipline internal/fsevents follows.
//
// Like internal/hotkey's RegisterEventHotKey, every Cocoa/WebKit call
// here runs on the main thread — dispatched via dispatch_sync onto the
// main queue, which is serviced by the same NSApplication run loop
// fyne.io/systray's Run() already owns. No second thread, no
// mainthread.Init: see internal/menubar's package doc for the fuller
// argument.
package panel

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

// ScryPanel is a borderless, non-activating panel: it can take key focus
// (so the WKWebView inside it can receive typed input) without stealing
// focus from whatever app the user was in until it's actually shown, and
// it hides itself on Escape or on losing key status, so it never lingers
// on top of other work.
@interface ScryPanel : NSPanel <NSWindowDelegate>
@end

@implementation ScryPanel
- (BOOL)canBecomeKeyWindow {
	return YES;
}
- (void)cancelOperation:(id)sender {
	[self orderOut:nil];
}
- (void)windowDidResignKey:(NSNotification *)notification {
	[self orderOut:nil];
}
@end

// createPanelC builds the panel and loads url into its WKWebView. Runs
// synchronously on the main queue because every AppKit/WebKit object
// created here must live on the thread that owns the run loop.
void* createPanelC(const char* url) {
	__block ScryPanel* panel = nil;
	dispatch_sync(dispatch_get_main_queue(), ^{
		NSRect frame = NSMakeRect(0, 0, 680, 420);
		NSUInteger style = NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel;
		panel = [[ScryPanel alloc] initWithContentRect:frame
		                                      styleMask:style
		                                        backing:NSBackingStoreBuffered
		                                          defer:NO];
		[panel setLevel:NSFloatingWindowLevel];
		[panel setOpaque:NO];
		[panel setHasShadow:YES];
		[panel setHidesOnDeactivate:YES];
		[panel setDelegate:panel];

		WKWebView* webView = [[WKWebView alloc] initWithFrame:frame];
		[panel setContentView:webView];

		NSURL* nsurl = [NSURL URLWithString:[NSString stringWithUTF8String:url]];
		if (nsurl != nil) {
			[webView loadRequest:[NSURLRequest requestWithURL:nsurl]];
		}
	});
	// Retained by the block's assignment to panel and returned as an
	// opaque handle; togglePanelC/closePanelC never touch anything but
	// window-level API on it, so no further bridging is needed.
	return (__bridge_retained void*)panel;
}

static void centerOnActiveScreen(ScryPanel* panel) {
	NSScreen* screen = [NSScreen mainScreen];
	if (screen == nil) {
		[panel center];
		return;
	}
	NSRect screenFrame = [screen frame];
	NSRect panelFrame = [panel frame];
	CGFloat x = screenFrame.origin.x + (screenFrame.size.width - panelFrame.size.width) / 2;
	CGFloat y = screenFrame.origin.y + (screenFrame.size.height - panelFrame.size.height) * 0.66; // above center, Spotlight-style
	[panel setFrameOrigin:NSMakePoint(x, y)];
}

void togglePanelC(void* p) {
	dispatch_sync(dispatch_get_main_queue(), ^{
		ScryPanel* panel = (__bridge ScryPanel*)p;
		if ([panel isVisible]) {
			[panel orderOut:nil];
			return;
		}
		centerOnActiveScreen(panel);
		[panel makeKeyAndOrderFront:nil];
		[NSApp activateIgnoringOtherApps:YES];
	});
}

void closePanelC(void* p) {
	dispatch_sync(dispatch_get_main_queue(), ^{
		ScryPanel* panel = (__bridge ScryPanel*)p;
		[panel orderOut:nil];
	});
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

type darwinPanel struct {
	ref unsafe.Pointer
}

// New creates a hidden panel loading url. The panel is not shown until
// the first Toggle call.
func New(url string) (Panel, error) {
	cURL := C.CString(url)
	defer C.free(unsafe.Pointer(cURL))

	ref := C.createPanelC(cURL)
	if ref == nil {
		return nil, errors.New("panel: failed to create panel")
	}
	return &darwinPanel{ref: ref}, nil
}

func (p *darwinPanel) Toggle() { C.togglePanelC(p.ref) }
func (p *darwinPanel) Close()  { C.closePanelC(p.ref) }
