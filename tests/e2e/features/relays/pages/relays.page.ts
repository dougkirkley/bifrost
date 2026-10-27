import { Locator, Page } from "@playwright/test";
import { BasePage } from "../../../core/pages/base.page";

/** Page object for /workspace/relays. */
export class RelaysPage extends BasePage {
  readonly createButton: Locator;
  readonly createFirstButton: Locator;
  readonly listItems: Locator;
  readonly editor: Locator;

  constructor(page: Page) {
    super(page);
    this.createButton = page.getByTestId("relays-create-button");
    this.createFirstButton = page.getByTestId("relays-create-first");
    this.listItems = page.getByTestId("relay-list-item");
    this.editor = page.getByTestId("relay-view");
  }

  async goto(): Promise<void> {
    await this.page.goto("/workspace/relays");
    await this.waitForPageLoad();
  }

  /** Opens the add sheet from either the empty state or the list. */
  async openCreate(): Promise<void> {
    if (await this.createFirstButton.isVisible().catch(() => false)) {
      await this.createFirstButton.click();
    } else {
      await this.createButton.click();
    }
  }

  async createEndpoint(name: string, baseUrl: string, credential: string): Promise<void> {
    await this.openCreate();
    await this.page.getByTestId("relay-name").fill(name);
    await this.page.getByTestId("relay-new-base-url").fill(baseUrl);
    await this.page.getByTestId("relay-credential").fill(credential);
    await this.page.getByTestId("relay-create").click();
    // Consume the creation toast so a later success-toast assertion cannot match this stale one.
    await this.waitForSuccessToast();
    await this.listItems.filter({ hasText: name }).first().waitFor();
  }

  async select(name: string): Promise<void> {
    await this.listItems.filter({ hasText: name }).first().click();
  }

  async openConfigTab(tab: string): Promise<void> {
    await this.page.getByTestId("relay-edit-config").click();
    await this.page.getByTestId(`relay-tab-${tab}`).click();
  }

  async deleteSelected(): Promise<void> {
    await this.page.getByTestId("relay-delete").click();
    await this.page.getByTestId("relay-delete-confirm").click();
  }
}