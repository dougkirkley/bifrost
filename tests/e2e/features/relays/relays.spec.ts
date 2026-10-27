import { expect, test } from "../../core/fixtures/base.fixture";

const API = "/api/relays";
const created: string[] = [];

test.describe("Relays", () => {
  test.afterEach(async ({ request }) => {
    for (const name of created.splice(0)) {
      await request.delete(`${API}/${name}`).catch(() => undefined);
    }
  });

  test("creates an endpoint, prices a route override and previews its cost", async ({
    relaysPage: page,
  }) => {
    const name = `e2e-pte-${Date.now()}`;
    created.push(name);
    await page.goto();
    await page.createEndpoint(name, "https://api.example.com", "sk-e2e-secret");
    await page.select(name);
    await expect(page.editor).toBeVisible();

    // Endpoint-level pricing lives on the General tab of the config sheet.
    await page.openConfigTab("general");
    await page.page.getByTestId("relay-usage-cost-per-request").fill("0.001");
    await page.page.getByTestId("relay-save-general").click();
    await expect(page.getToast("success")).toBeVisible();
    await page.page.keyboard.press("Escape");

    // A route whose override adds a per-unit charge read from the response.
    await page.page.getByTestId("relay-routes-add").click();
    await page.page.getByTestId("relay-route-path").fill("/search");
    // The override is collapsed until opened.
    await expect(page.page.getByTestId("relay-route-usage-units-path")).toBeHidden();
    await page.page.getByTestId("relay-route-pricing-toggle").click();
    await page.page.getByTestId("relay-route-usage-units-path").fill("usage.credits");
    await page.page.getByTestId("relay-route-usage-cost-per-unit").fill("0.008");
    await page.page.getByTestId("relay-route-save").click();
    await expect(page.page.getByTestId("relay-route-row-0")).toContainText("/search");
    await expect(page.page.getByTestId("relay-route-row-0")).toContainText("override");
    await expect(page.page.getByTestId("relay-route-enabled-switch-0")).toBeChecked();

    // Test a call against the saved mapping: 0.001 + 4 * 0.008.
    await page.page.getByTestId("relay-preview-toggle").click();
    await expect(page.page.getByTestId("relay-preview-sheet")).toBeVisible();
    await page.page.getByTestId("relay-preview-path").fill("/search");
    await page.page.getByTestId("relay-preview-response-body").fill('{"usage":{"credits":4}}');
    await page.page.getByTestId("relay-preview-run").click();
    await expect(page.page.getByTestId("relay-preview-cost")).toHaveText("$0.033");
    await expect(page.page.getByTestId("relay-preview-formula")).toContainText("4 × $0.008");
  });

  test("warns when the endpoint relays any path", async ({ relaysPage: page }) => {
    const name = `e2e-pte-open-${Date.now()}`;
    created.push(name);
    await page.goto();
    await page.createEndpoint(name, "https://api.example.com", "sk-e2e-secret");
    await page.select(name);
    await page.openConfigTab("general");
    await page.page.getByTestId("relay-skip-route-check").click();
    await expect(page.page.getByTestId("relay-all-routes-warning")).toBeVisible();
  });

  test("deletes an endpoint", async ({ relaysPage: page, request }) => {
    const name = `e2e-pte-del-${Date.now()}`;
    created.push(name);
    await page.goto();
    await page.createEndpoint(name, "https://api.example.com", "sk-e2e-secret");
    await page.select(name);
    await page.deleteSelected();
    await expect(page.getToast("success")).toBeVisible();
    const res = await request.get(`${API}/${name}`);
    expect(res.status()).toBe(404);
  });

  test("virtual key sheet offers relay access", async ({ page }) => {
    await page.goto("/workspace/virtual-keys");
    await page
      .getByTestId("create-vk-btn")
      .or(page.getByRole("button", { name: /add virtual key|create virtual key/i }))
      .first()
      .click();
    await expect(page.getByTestId("vk-relays-editor")).toBeVisible();
    await page.getByTestId("vk-relays-allow-all").click();
    await expect(page.getByTestId("vk-relays-allow-all")).toHaveAttribute("data-state", "checked");
  });

  test("relay logs page loads", async ({ page }) => {
    await page.goto("/workspace/relay-logs");
    await expect(
      page.getByTestId("relay-logs-page").or(page.getByTestId("relay-logs-empty")),
    ).toBeVisible();
  });
});