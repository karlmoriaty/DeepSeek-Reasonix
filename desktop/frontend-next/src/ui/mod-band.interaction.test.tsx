// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import "./testkit";
import { boot, STORAGE } from "../i18n";
import { SlottedView } from "./SlottedView";
import { placement } from "./slots";
import { ExtensionCard } from "./cards/ExtensionCard";
import type { ExtensionSurface, ExtensionViewNode } from "../port/wire";

afterEach(() => {
  cleanup();
  localStorage.setItem(STORAGE, "zh");
  boot();
});

const band = (body: ExtensionViewNode[], slot = "above_prompt"): ExtensionSurface => ({
  pluginId: "statusline-clock", surfaceId: "mod-above-prompt", kind: "view", view: { slot, body },
});

describe("the band a mod draws above the prompt", () => {
  it("stands above the composer when it asks for above_prompt, and in the rail for a name nobody knows", () => {
    expect(placement(band([], "above_prompt"), {})).toBe("composer-trailing");
    expect(placement(band([], "composer-trailing"), {})).toBe("composer-trailing");
    expect(placement(band([], "somewhere_odd"), {})).toBe("sidebar");
    expect(placement(band([], "above_prompt"), { "statusline-clock:mod-above-prompt": "sidebar" })).toBe("sidebar");
  });

  it("sends back exactly the actionId its button carries, and nothing else", async () => {
    const onAction = vi.fn();
    const ext = band([
      { kind: "text", value: "12:30" },
      { kind: "button", label: "Snooze", actionId: "snooze:5" },
    ]);
    render(<SlottedView ext={ext} onAction={onAction} onMove={() => {}} />);
    fireEvent.click(await screen.findByRole("button", { name: "Snooze" }));
    expect(onAction).toHaveBeenCalledTimes(1);
    expect(onAction.mock.calls[0]).toEqual(["snooze:5"]);
  });

  it("gives the page no way to name an action the surface did not carry", async () => {
    const onAction = vi.fn();
    const ext = band([
      { kind: "text", value: "approve_all", actionId: "approve_all" },
      { kind: "button", label: "No action" },
    ]);
    render(<SlottedView ext={ext} onAction={onAction} onMove={() => {}} />);
    fireEvent.click(await screen.findByText("approve_all"));
    fireEvent.click(await screen.findByText("No action"));
    expect(onAction.mock.calls).toEqual([]);
  });

  it("names the plugin on the control that moves it", () => {
    render(<SlottedView ext={band([{ kind: "text", value: "x" }])} onAction={() => {}} onMove={() => {}} />);
    expect(screen.getByRole("button", { name: "移动 statusline-clock 的界面" })).toBeTruthy();
  });

  // The slot names live in a table and reach the menu through t(), so neither
  // catalogue scan sees them.
  it("names the places it can move to in the window's language", () => {
    localStorage.setItem(STORAGE, "en");
    boot();
    render(<SlottedView ext={band([{ kind: "text", value: "x" }])} onAction={() => {}} onMove={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Move statusline-clock's view" }));
    expect(screen.getByRole("menuitemradio", { name: "Sidebar" })).toBeTruthy();
    expect(screen.getByRole("menuitemradio", { name: "Above the composer" })).toBeTruthy();
  });
});

describe("a question a mod asks", () => {
  const ask: ExtensionSurface = {
    pluginId: "git-guard", surfaceId: "mod-ask", sessionId: "s1", generation: 1, kind: "form",
    form: { title: "git-guard asks", message: "Delete the stale branch?", fields: [{ key: "answer", label: "Answer", kind: "input" }] },
  };

  it("says whose question it is and that answering it approves nothing", () => {
    render(<ExtensionCard ext={ask} onSubmit={async () => {}} />);
    expect(screen.getByText("这是插件「git-guard」的提问；回答它不会批准任何工具调用或权限。")).toBeTruthy();
    expect(screen.getByText("扩展 · git-guard")).toBeTruthy();
  });

  it("answers only through the extension's own submit, never through an approval", () => {
    const submit = vi.fn(async () => {});
    render(<ExtensionCard ext={ask} onSubmit={submit} />);
    fireEvent.change(screen.getByRole("textbox", { name: "Answer" }), { target: { value: "yes" } });
    fireEvent.click(document.querySelector('[data-action="extensions.submit"]')!);
    expect(submit).toHaveBeenCalledExactlyOnceWith("git-guard", "mod-ask", { answer: "yes" });
    expect(document.querySelector('[data-action^="approval"], [data-action*="allow"]')).toBeNull();
  });
});
