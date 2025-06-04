import React from "react";
import TraceViewer, { TraceViewProps, TraceViewerState } from "./trace_viewer";
import { Profile, TraceEvent } from "./trace_events";
import Panel from "./trace_viewer_panel"; // Will be mocked
import router from "../router/router"; // Will be mocked
import { AnimationLoop } from "../util/animation_loop"; // Will be mocked

// Mock Panel
jest.mock("./trace_viewer_panel");

// Mock router
jest.mock("../router/router", () => ({
  setQueryParam: jest.fn(),
  navigateTo: jest.fn(),
}));

// Mock AnimationLoop
jest.mock("../util/animation_loop", () => ({
  AnimationLoop: jest.fn().mockImplementation(() => ({
    start: jest.fn(),
    stop: jest.fn(),
    update: jest.fn(),
  })),
}));

// Mock ResizeObserver
global.ResizeObserver = jest.fn().mockImplementation(() => ({
  observe: jest.fn(),
  unobserve: jest.fn(),
  disconnect: jest.fn(),
}));

// Mock window.getComputedStyle
window.getComputedStyle = jest.fn().mockReturnValue({
  fontFamily: "Arial",
});

const sampleEvent1: TraceEvent = {
  pid: 1,
  tid: 1,
  ts: 100, // 100us
  ph: "X",
  cat: "category1",
  name: "Test Event One",
  dur: 50, // 50us
  tdur: 50,
  tts: 100,
  out: "",
  args: {},
  id: "event1",
};

const sampleEvent2: TraceEvent = {
  pid: 1,
  tid: 1,
  ts: 200, // 200us
  ph: "X",
  cat: "category2",
  name: "Test Event Two",
  dur: 70, // 70us
  tdur: 70,
  tts: 200,
  out: "",
  args: {},
  id: "event2",
};

const sampleEvent3: TraceEvent = {
  pid: 1,
  tid: 1,
  ts: 300, // 300us
  ph: "X",
  cat: "category1",
  name: "Another One", // Different name for filtering
  dur: 60, // 60us
  tdur: 60,
  tts: 300,
  out: "",
  args: {},
  id: "event3",
};

const sampleProfile: Profile = {
  traceEvents: [sampleEvent1, sampleEvent2, sampleEvent3],
};

// Helper to create a TraceViewer instance with props
// This is a simplified approach. Ideally, React Testing Library would be used.
const createInstance = (props: Partial<TraceViewProps> = {}) => {
  const defaultProps: TraceViewProps = {
    profile: sampleProfile,
    ...props,
  };
  // This is not standard React testing. We're directly instantiating.
  // For proper testing, we'd render into a DOM and interact.
  // However, given the tool limitations, this allows us to call methods and check state.
  const instance = new TraceViewer(defaultProps);
  // Simulate componentDidMount essentials if not using RTL's render
  instance.componentDidMount();
  // We need to manually assign the mocked panels if TraceViewer creates them internally
  // This is a bit of a hack due to not using a proper testing library.
  // @ts-ignore
  instance.panels = instance.model.panels.map(() => new Panel(jest.fn() as any, jest.fn() as any, "Arial"));
  return instance;
};


describe("TraceViewer", () => {
  let instance: TraceViewer;

  beforeEach(() => {
    jest.clearAllMocks();
    instance = createInstance();
    // @ts-ignore
    instance.panels.forEach((panelMock) => {
      // Setup default mock panel properties needed for most tests
      // @ts-ignore
      panelMock.container = {
        clientWidth: 1000,
        clientHeight: 200,
        scrollLeft: 0,
        scrollTop: 0,
        scrollWidth: 2000, // Example: content is wider than clientWidth
        scrollHeight: 400,
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 1000, bottom: 200, width: 1000, height: 200, x:0, y:0, toJSON: () => ({}) }),
        getElementsByClassName: (name: string) => {
          if (name === 'sizer') return [{ style: { width: '', height: ''}} as HTMLDivElement];
          return [];
        },
        addEventListener: jest.fn(),
        removeEventListener: jest.fn(),
      };
      // @ts-ignore
      panelMock.canvas = {
        getContext: () => ({
          scale: jest.fn(),
          clearRect: jest.fn(),
          fillRect: jest.fn(),
          strokeRect: jest.fn(), // Important for highlighting test
          beginPath: jest.fn(),
          moveTo: jest.fn(),
          lineTo: jest.fn(),
          closePath: jest.fn(),
          fill: jest.fn(),
          stroke: jest.fn(),
          fillText: jest.fn(),
        }),
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 1000, bottom: 200, width: 1000, height: 200, x:0, y:0, toJSON: () => ({}) }),
      };
      // @ts-ignore
      panelMock.resize = jest.fn();
      // @ts-ignore
      panelMock.draw = jest.fn();
      // @ts-ignore
      panelMock.model = { events: [], xMax: 500 }; // Provide a basic model structure
       // @ts-ignore
      panelMock.getHoveredEvent = jest.fn().mockReturnValue(null);

    });
     // @ts-ignore
    instance.canvasXPerModelX.value = 0.1; // Default scale for tests
    // @ts-ignore
    instance.update();
  });

  it("should instantiate", () => {
    expect(instance).toBeInstanceOf(TraceViewer);
  });

  // More tests will go here
});

describe("TraceViewer - Filtering", () => {
  let instance: TraceViewer;

  // Using the global beforeEach from the parent describe block for panel setup
  beforeEach(() => {
    // This will re-run the parent beforeEach, resetting instance and mocks
    // Reset mocks before each test
    jest.clearAllMocks();
    instance = createInstance();
     // @ts-ignore
    instance.panels.forEach((panelMock) => {
      // Setup default mock panel properties needed for most tests
      // @ts-ignore
      panelMock.container = {
        clientWidth: 1000,
        clientHeight: 200,
        scrollLeft: 0,
        scrollTop: 0,
        scrollWidth: 2000, // Example: content is wider than clientWidth
        scrollHeight: 400,
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 1000, bottom: 200, width: 1000, height: 200, x:0, y:0, toJSON: () => ({}) }),
        getElementsByClassName: (name: string) => {
          if (name === 'sizer') return [{ style: { width: '', height: ''}} as HTMLDivElement];
          return [];
        },
        addEventListener: jest.fn(),
        removeEventListener: jest.fn(),
      };
      // @ts-ignore
      panelMock.canvas = {
        getContext: () => ({
          scale: jest.fn(),
          clearRect: jest.fn(),
          fillRect: jest.fn(),
          strokeRect: jest.fn(),
          beginPath: jest.fn(),
          moveTo: jest.fn(),
          lineTo: jest.fn(),
          closePath: jest.fn(),
          fill: jest.fn(),
          stroke: jest.fn(),
          fillText: jest.fn(),
        }),
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 1000, bottom: 200, width: 1000, height: 200, x:0, y:0, toJSON: () => ({}) }),
      };
      // @ts-ignore
      panelMock.resize = jest.fn();
      // @ts-ignore
      panelMock.draw = jest.fn();
      // @ts-ignore
      panelMock.model = { events: [sampleEvent1, sampleEvent2, sampleEvent3], xMax: sampleEvent3.ts + sampleEvent3.dur + 100 };
       // @ts-ignore
      panelMock.getHoveredEvent = jest.fn().mockReturnValue(null);
    });
     // @ts-ignore
    instance.canvasXPerModelX.value = 1; // Set a default scale, e.g., 1 pixel per microsecond for simplicity
    // @ts-ignore
    instance.update();
  });


  it("should filter spans and update selection (match)", () => {
    // @ts-ignore - Accessing private method for test
    instance.updateFilter("Test Event");
    const state = instance.state as TraceViewerState;
    // Assuming sampleEvent1 and sampleEvent2 match "Test Event"
    // And they exist in the panel.model.events for the filter to find them.
    expect(state.matchedSpans.length).toBe(2);
    expect(state.matchedSpans).toEqual(expect.arrayContaining([sampleEvent1, sampleEvent2]));
    expect(state.selectedSpanIndex).toBe(0);
    expect(router.setQueryParam).toHaveBeenCalledWith("timingFilter", "Test Event");
  });

  it("should filter spans case-insensitively", () => {
    // @ts-ignore
    instance.updateFilter("test event one");
    const state = instance.state as TraceViewerState;
    expect(state.matchedSpans.length).toBe(1);
    expect(state.matchedSpans[0]).toBe(sampleEvent1);
    expect(state.selectedSpanIndex).toBe(0);
  });

  it("should handle filter with no matches", () => {
    // @ts-ignore
    instance.updateFilter("NoMatchForThis");
    const state = instance.state as TraceViewerState;
    expect(state.matchedSpans.length).toBe(0);
    expect(state.selectedSpanIndex).toBe(-1);
  });

  it("should clear filter and selection", () => {
    // First, apply a filter
    // @ts-ignore
    instance.updateFilter("Test Event");
    expect((instance.state as TraceViewerState).matchedSpans.length).toBe(2);

    // Then, clear it
    // @ts-ignore
    instance.updateFilter("");
    const state = instance.state as TraceViewerState;
    expect(state.matchedSpans.length).toBe(0);
    expect(state.selectedSpanIndex).toBe(-1);
    expect(router.setQueryParam).toHaveBeenCalledWith("timingFilter", "");
  });
});

describe("TraceViewer - Scrolling", () => {
  let instance: TraceViewer;
   let scrollToSelectedSpanSpy: jest.SpyInstance;

  beforeEach(() => {
    jest.clearAllMocks();
    instance = createInstance();
     // @ts-ignore
    instance.panels.forEach((panelMock) => {
        // @ts-ignore
      panelMock.container = {
        clientWidth: 1000,
        clientHeight: 200,
        scrollLeft: 0,
        scrollTop: 0,
        scrollWidth: 5000, // Large enough scroll width
        scrollHeight: 400,
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 1000, bottom: 200, width: 1000, height: 200, x:0, y:0, toJSON: () => ({}) }),
        getElementsByClassName: (name: string) => {
          if (name === 'sizer') return [{ style: { width: '', height: ''}} as HTMLDivElement];
          return [];
        },
        addEventListener: jest.fn(),
        removeEventListener: jest.fn(),
      };
       // @ts-ignore
      panelMock.canvas = { getContext: () => ({ scale: jest.fn(), clearRect: jest.fn(), /* other methods */ }) };
       // @ts-ignore
      panelMock.resize = jest.fn();
       // @ts-ignore
      panelMock.draw = jest.fn();
       // @ts-ignore
      panelMock.model = { events: [sampleEvent1, sampleEvent2, sampleEvent3], xMax: 5000 };
    });
    // @ts-ignore
    instance.canvasXPerModelX.value = 1; // 1 pixel per microsecond for simpler calculation
    // @ts-ignore
    instance.update();
    // @ts-ignore
    scrollToSelectedSpanSpy = jest.spyOn(instance, 'scrollToSelectedSpan');
  });

  it("should scroll to the selected span when filter matches", () => {
    // @ts-ignore
    instance.updateFilter("Test Event One"); // Matches sampleEvent1 (ts: 100)

    expect(scrollToSelectedSpanSpy).toHaveBeenCalled();

    // Verify panel's scrollLeft was attempted to be set
    // targetX = selectedSpan.ts * this.canvasXPerModelX.value = 100 * 1 = 100
    // scrollX = targetX - panelWidth / 2 = 100 - 1000 / 2 = 100 - 500 = -400
    // Clamped to 0
    // @ts-ignore
    expect(instance.panels[0].scrollX).toBe(0);
    // @ts-ignore
    expect(instance.panels[0].container.scrollLeft).toBe(0);
  });

  it("should scroll to a different span if its start time requires different scrolling", () => {
    // @ts-ignore
    instance.panels.forEach(p => p.container.clientWidth = 200); // smaller panel width
    // @ts-ignore
    instance.canvasXPerModelX.value = 1;
    // @ts-ignore
    instance.update(); // re-run update after changing panel/scale

    // @ts-ignore
    instance.updateFilter("Event Two"); // Matches sampleEvent2 (ts: 200)
    expect(scrollToSelectedSpanSpy).toHaveBeenCalled();

    // targetX = 200 * 1 = 200
    // scrollX = 200 - 200 / 2 = 200 - 100 = 100
    // @ts-ignore
    expect(instance.panels[0].scrollX).toBe(100);
    // @ts-ignore
    expect(instance.panels[0].container.scrollLeft).toBe(100);
  });


  it("should not scroll if no span is selected", () => {
    // @ts-ignore
    instance.updateFilter("NoMatchHere");
    expect(scrollToSelectedSpanSpy).toHaveBeenCalled(); // It's called, but should do nothing internally
     // Check that scrollX remains as it was (or 0 if nothing ever set it)
     // @ts-ignore
    expect(instance.panels[0].scrollX).toBe(0);
  });
});

describe("TraceViewer - Keyboard Navigation", () => {
  let instance: TraceViewer;
  let scrollToSelectedSpanSpy: jest.SpyInstance;
  let mockFilterInputElement: HTMLInputElement;

  beforeEach(() => {
    jest.clearAllMocks();
    instance = createInstance();
     // @ts-ignore
    instance.panels.forEach((panelMock) => {
      // @ts-ignore
      panelMock.container = {
        clientWidth: 1000, clientHeight: 200, scrollLeft: 0, scrollTop: 0, scrollWidth: 2000, scrollHeight: 400,
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 1000, bottom: 200, width: 1000, height: 200, x:0, y:0, toJSON: () => ({}) }),
        getElementsByClassName: (name: string) => (name === 'sizer' ? [{ style: { width: '', height: ''}} as HTMLDivElement] : []),
        addEventListener: jest.fn(), removeEventListener: jest.fn(),
      };
      // @ts-ignore
      panelMock.canvas = { getContext: () => ({ scale: jest.fn(), clearRect: jest.fn() }) };
      // @ts-ignore
      panelMock.resize = jest.fn(); panelMock.draw = jest.fn();
      // @ts-ignore
      panelMock.model = { events: [sampleEvent1, sampleEvent2, sampleEvent3], xMax: 500 };
    });
    // @ts-ignore
    instance.canvasXPerModelX.value = 1;
    // @ts-ignore
    instance.update();
    // @ts-ignore
    scrollToSelectedSpanSpy = jest.spyOn(instance, 'scrollToSelectedSpan');

    // Mock the filter input element
    mockFilterInputElement = document.createElement("input");
    // @ts-ignore Assign mock ref for filterInputRef
    instance.filterInputRef = { current: mockFilterInputElement };

    // Initial filter to have some matchedSpans
    // @ts-ignore
    instance.updateFilter("Test Event"); // Matches sampleEvent1 and sampleEvent2
  });

  it("should select next span on Enter key", () => {
    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(0); // Initially first item

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", preventDefault: jest.fn(), target: null } as KeyboardEvent);

    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(1);
    expect(scrollToSelectedSpanSpy).toHaveBeenCalledTimes(1); // Called once after keydown
  });

  it("should wrap to first span on Enter key if at end of list", () => {
    // @ts-ignore
    instance.setState({ selectedSpanIndex: 1 }); // Manually set to last of 2 matched spans

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", preventDefault: jest.fn(), target: null } as KeyboardEvent);

    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(0);
    expect(scrollToSelectedSpanSpy).toHaveBeenCalledTimes(1);
  });

  it("should select previous span on Shift+Enter key", () => {
    // @ts-ignore
    instance.setState({ selectedSpanIndex: 1 }); // Start at the second matched span

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", shiftKey: true, preventDefault: jest.fn(), target: null } as KeyboardEvent);

    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(0);
    expect(scrollToSelectedSpanSpy).toHaveBeenCalledTimes(1);
  });

  it("should wrap to last span on Shift+Enter key if at start of list", () => {
    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(0); // Initially first item

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", shiftKey: true, preventDefault: jest.fn(), target: null } as KeyboardEvent);

    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(1); // Wraps to last of 2 matched
    expect(scrollToSelectedSpanSpy).toHaveBeenCalledTimes(1);
  });

  it("should do nothing if no spans are matched", () => {
    // @ts-ignore
    instance.updateFilter("NoMatchHere"); // Clear matchedSpans
    const initialIndex = (instance.state as TraceViewerState).selectedSpanIndex; // should be -1
    expect(initialIndex).toBe(-1);

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", preventDefault: jest.fn(), target: null } as KeyboardEvent);

    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(initialIndex);
    expect(scrollToSelectedSpanSpy).not.toHaveBeenCalled(); // scrollToSelectedSpan is part of handleKeyDown, but it bails early
  });

  it("should ignore keyboard navigation if filter input is focused", () => {
    const initialIndex = (instance.state as TraceViewerState).selectedSpanIndex;

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", preventDefault: jest.fn(), target: mockFilterInputElement } as KeyboardEvent);

    expect((instance.state as TraceViewerState).selectedSpanIndex).toBe(initialIndex);
    expect(scrollToSelectedSpanSpy).not.toHaveBeenCalled();
  });

   it("should prevent default action for Enter/Shift+Enter", () => {
    const preventDefaultSpy = jest.fn();
    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", preventDefault: preventDefaultSpy, target: null } as KeyboardEvent);
    expect(preventDefaultSpy).toHaveBeenCalled();

    preventDefaultSpy.mockClear();
    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", shiftKey: true, preventDefault: preventDefaultSpy, target: null } as KeyboardEvent);
    expect(preventDefaultSpy).toHaveBeenCalled();
  });

});

describe("TraceViewer - Highlighting", () => {
  let instance: TraceViewer;

  beforeEach(() => {
    jest.clearAllMocks();
    instance = createInstance();
    // @ts-ignore
    instance.panels.forEach((panelMock) => {
      // @ts-ignore
      panelMock.container = {
        clientWidth: 1000, clientHeight: 200, scrollLeft: 0, scrollTop: 0, scrollWidth: 2000, scrollHeight: 400,
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 1000, bottom: 200, width: 1000, height: 200, x:0, y:0, toJSON: () => ({}) }),
        getElementsByClassName: (name: string) => (name === 'sizer' ? [{ style: { width: '', height: ''}} as HTMLDivElement] : []),
        addEventListener: jest.fn(), removeEventListener: jest.fn(),
      };
      // @ts-ignore
      panelMock.canvas = { getContext: () => ({ scale: jest.fn(), clearRect: jest.fn(), strokeRect: jest.fn() }) }; // Ensure strokeRect is mocked
      // @ts-ignore
      panelMock.resize = jest.fn();
      // @ts-ignore
      panelMock.draw = jest.fn(); // We will check if draw is called, implying a repaint
      // @ts-ignore
      panelMock.model = { events: [sampleEvent1, sampleEvent2, sampleEvent3], xMax: 500 };
      // @ts-ignore
      panelMock.selectedEvent = null; // Ensure it starts as null
    });
    // @ts-ignore
    instance.canvasXPerModelX.value = 1;
    // @ts-ignore
    instance.update(); // This will call panel.draw()
  });

  it("should set selectedEvent on panels when a filter matches and selection occurs", () => {
    // @ts-ignore
    instance.updateFilter("Test Event One"); // Matches sampleEvent1, selectedSpanIndex becomes 0

    // The update method in TraceViewer calls panel.draw() which should use the new selectedEvent.
    // We need to ensure panel.selectedEvent was set correctly before draw was called.
    // The actual setting happens in instance.update()

    // @ts-ignore
    const selectedInState = instance.state.matchedSpans[instance.state.selectedSpanIndex];
    expect(selectedInState).toBe(sampleEvent1);

    // @ts-ignore
    instance.panels.forEach(p => {
        // @ts-ignore
      expect(p.selectedEvent).toBe(sampleEvent1);
      expect(p.draw).toHaveBeenCalled(); // draw is called in update()
    });
  });

  it("should update selectedEvent on panels when keyboard navigation changes selection", () => {
    // @ts-ignore
    instance.updateFilter("Test Event"); // Matches sampleEvent1 (index 0) and sampleEvent2 (index 1)
    // @ts-ignore
    expect(instance.panels[0].selectedEvent).toBe(sampleEvent1); // Initial selection

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", preventDefault: jest.fn(), target: null } as KeyboardEvent); // Moves to sampleEvent2

    // @ts-ignore
    const selectedInState = instance.state.matchedSpans[instance.state.selectedSpanIndex];
    expect(selectedInState).toBe(sampleEvent2);

    // The call to scrollToSelectedSpan in handleKeyDown calls instance.update(), which updates panels
    // @ts-ignore
    instance.panels.forEach(p => {
        // @ts-ignore
      expect(p.selectedEvent).toBe(sampleEvent2);
      expect(p.draw).toHaveBeenCalled();
    });
  });

  it("should set selectedEvent to null on panels if filter results in no selection", () => {
    // @ts-ignore
    instance.updateFilter("Test Event One"); // Selects sampleEvent1
    // @ts-ignore
    expect(instance.panels[0].selectedEvent).toBe(sampleEvent1);

    // @ts-ignore
    instance.updateFilter("NoMatchHere"); // Clears selection

    // @ts-ignore
    expect(instance.state.selectedSpanIndex).toBe(-1);
    // @ts-ignore
    instance.panels.forEach(p => {
        // @ts-ignore
      expect(p.selectedEvent).toBeNull();
      expect(p.draw).toHaveBeenCalled();
    });
  });
   it("should call panel.draw when selected event changes, implying highlight update", () => {
    // @ts-ignore
    const panelDrawSpies = instance.panels.map(p => jest.spyOn(p, 'draw'));

    // @ts-ignore
    instance.updateFilter("Test Event One"); // Changes selection, calls update -> draw
    panelDrawSpies.forEach(spy => expect(spy).toHaveBeenCalled());

    panelDrawSpies.forEach(spy => spy.mockClear());

    // @ts-ignore
    instance.handleKeyDown({ key: "Enter", preventDefault: jest.fn(), target: null } as KeyboardEvent); // Changes selection, calls update -> draw
    panelDrawSpies.forEach(spy => expect(spy).toHaveBeenCalled());
  });
});
