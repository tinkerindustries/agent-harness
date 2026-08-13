---
title: Account and Billing
source: https://platform.kimi.ai/docs/guide/account-and-payments
fetched: 2026-08-13
---

# Account and Billing

<AccordionGroup>
  <Accordion title="How do I top up my account?">
    * Individual users: Go to the user top-up page and complete an online payment. Online top-ups support WeChat Pay and Alipay QR code payments. After the payment succeeds, your account tier will be adjusted based on your accumulated top-up amount.
    * Business users: Please contact the Kimi sales team or follow the payment methods supported in your account. Supported options may include online payment or bank transfer, depending on your account region and billing setup. After the payment is received, your account tier will be adjusted based on your accumulated top-up amount.
  </Accordion>

  <Accordion title="How do I control costs and set a spending limit?">
    When using large models for code generation, the model may need multiple attempts to produce the expected code because of the randomness and complexity of generation. Programming tools may automatically perform multiple rounds of retries and calls, which can cause token usage to grow quickly. To better control costs and improve the usage experience, we recommend the following:

    * **Budget control**
      * **Set a daily spending limit**: Before use, go to [Kimi Open Platform project settings](https://platform.kimi.ai/console/projects/settings) and configure a project daily spending budget. Once the budget limit is reached, the system will automatically reject all API requests under that project. Note that due to billing latency, the limit may take about 10 minutes to take effect. For setup instructions, see [Organization Management Best Practices](org-best-practice.md).
      * **Balance alerts**: We recommend enabling account balance alerts. When your account balance falls below the preset amount, the system will notify you so you can top up in time.
    * **Usage recommendations**
      * Start with a short context and a clear prompt for testing, then gradually add the complete business context.
      * **Continuous monitoring**: Keep monitoring your programming tool while it is running, handle abnormal situations promptly, and avoid unnecessary resource consumption caused by infinite loops or excessive retries.
      * **Model selection**: If cost is a concern, you can use the `kimi-k2.6` model.
  </Accordion>

  <Accordion title="How is Kimi K3 billed?">
    Kimi K3 offers a 1M-token context and uses flat pay-as-you-go pricing — there is no tiering by context length. Input (with separate rates for cache hits and misses) and output are billed at uniform per-token prices. See [Kimi K3 pricing](../pricing/chat-k3.md).
  </Accordion>

  <Accordion title="How do I increase my API rate limits?">
    To ensure fair resource allocation and prevent abuse, rate limits are currently based on the account's accumulated top-up amount. For higher limits, please submit the [rate limit increase form](https://platform.kimi.ai/contact-sales). For more details, see the [Top-up and Rate Limits](https://platform.kimi.ai/docs/pricing/limits) page.
  </Accordion>

  <Accordion title="Are dedicated products and services available for business customers?">
    * The Kimi sales team can provide additional resources and support for business customers using the Kimi API. Please fill out the form at [https://platform.kimi.ai/contact-sales](https://platform.kimi.ai/contact-sales) to contact sales.
    * Kimi Assistant now offers Kimi Business membership benefits. Visit [https://www.kimi.ai/membership/pricing](https://www.kimi.ai/membership/pricing) to subscribe online.
  </Accordion>

  <Accordion title="How do I request an invoice?">
    * The platform supports issuing invoices based on either consumed amount or top-up amount. Please submit an invoice request online in [Invoice Management](https://platform.kimi.ai/console/invoice).
    * Supported invoice titles and requirements may vary by account type and billing setup. Please follow the options shown on the invoice request page.
    * Invoice type and tax rate: The issuing entity is Beijing Moonshot AI Technology Co., Ltd.; the service category is information technology services; the tax-inclusive rate is 6%.
  </Accordion>

  <Accordion title="Can I log in with an account password?">
    * Yes. After setting a password, you can log in with either your phone number and password or your account name and password. See [Account Password Settings](https://platform.kimi.ai/profile).
  </Accordion>

  <Accordion title="Can I change the phone number bound to my account?">
    * Yes. The target phone number must not have been registered on Kimi Open Platform or Kimi Assistant.
  </Accordion>

  <Accordion title="Can I delete my account?">
    * Account deletion is not currently supported.
  </Accordion>
</AccordionGroup>
