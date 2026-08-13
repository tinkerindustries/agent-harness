---
title: Manage Email and Google Sign-In
source: https://platform.kimi.ai/docs/guide/account-security-and-sign-in
fetched: 2026-08-13
---

# Manage Email and Google Sign-In

> Bind or update an email address, connect a Google account, and understand how sign-in methods affect your Kimi Platform account.

Kimi Platform supports two account sign-in methods:

* **Email sign-in:** Receive a verification code at your email address. After the address is added under **Email Binding**, it can also be used for password reset.
* **Sign in with Google:** Authorize Kimi Platform through Google's OAuth sign-in flow. The connected identity appears under **Google Account Binding**.

<Note>
  **Email Binding and Google Account Binding are separate credentials.** A Gmail address used for email verification is not automatically the same credential as a Google account with the same address.
</Note>

## Open account security settings

1. Sign in to [Kimi Platform](https://platform.kimi.ai/).
2. Select **User Center** in the top navigation. You can also open [Account Settings](https://platform.kimi.ai/profile) directly.
3. Under **Security Information**, locate **Email Binding** and **Google Account Binding**.

<img src="https://mintcdn.com/moonshotai/S5JdGHJXYeVuqorb/assets/pics/account-security/user-center-entry.png?fit=max&auto=format&n=S5JdGHJXYeVuqorb&q=85&s=25ee0f422c91c641ed4207791dc9c30f" alt="Select User Center in the Kimi Platform navigation" width="2944" height="135" data-path="assets/pics/account-security/user-center-entry.png" />

From this section, you can manage the following settings:

| Setting                    | Available actions                     | What it controls                              |
| -------------------------- | ------------------------------------- | --------------------------------------------- |
| **Email Binding**          | **Bind Email** or **Modify**          | Email verification sign-in and password reset |
| **Google Account Binding** | **Bind Google Account** or **Unbind** | OAuth-based **Sign in with Google**           |

## Bind or change an email address

If no email address is bound:

1. Select **Bind Email**.
2. Enter the email address you want to use.
3. Request and enter the verification code sent to that address.
4. Complete verification.

If an email address is already bound, select **Modify**. You will need to verify the current address and the new address. The new address must not already be bound to another Kimi Platform account.

The following state shows an account with an email address bound and no connected Google account:

<img src="https://mintcdn.com/moonshotai/S5JdGHJXYeVuqorb/assets/pics/account-security/email-bound-google-unbound.png?fit=max&auto=format&n=S5JdGHJXYeVuqorb&q=85&s=d71e6228b98f176cad660b094e6e8928" alt="An email address is bound and no Google account is connected" width="2960" height="430" data-path="assets/pics/account-security/email-bound-google-unbound.png" />

## Bind a Google account

1. Under **Google Account Binding**, select **Bind Google Account**.
2. Choose a Google account and complete Google's authorization flow.
3. Return to User Center and confirm that the Google account appears under **Google Account Binding**.

After both methods are configured, you can sign in with the bound email address or use **Sign in with Google**.

<img src="https://mintcdn.com/moonshotai/S5JdGHJXYeVuqorb/assets/pics/account-security/email-and-google-bound.png?fit=max&auto=format&n=S5JdGHJXYeVuqorb&q=85&s=32fe3236db23848c1da2cc948e15a7a3" alt="An email address and a Google account are both connected" width="2860" height="330" data-path="assets/pics/account-security/email-and-google-bound.png" />

<Warning>
  **Binding fails if the selected Google identity is already connected to another Kimi Platform account.** Choose a different Google account or remove that Google Account Binding from the other account first.
</Warning>

## How sign-in order affects account bindings

The order in which you use email verification and Google sign-in can affect what appears in User Center.

### Email verification first, then Google

When you first sign in with a verification code sent to an email address, the address appears under **Email Binding**. **Google Account Binding** remains empty.

If you later use **Sign in with Google** with the Google account that has the same email address, Kimi Platform automatically adds that Google identity to the existing account, provided that:

* The Kimi Platform account does not already have a different Google account bound.
* The selected Google identity is not bound to another Kimi Platform account.

### Google first, then email verification

When you first use **Sign in with Google**, the Google identity appears under **Google Account Binding**. **Email Binding** may remain empty.

Signing in later with an email verification code sent to the same address does not automatically add that address under **Email Binding**. If User Center still shows only the Google account, select **Bind Email** to add an email credential explicitly.

<img src="https://mintcdn.com/moonshotai/S5JdGHJXYeVuqorb/assets/pics/account-security/google-only.png?fit=max&auto=format&n=S5JdGHJXYeVuqorb&q=85&s=fb6309f445599839feb30eb3507f99b7" alt="Bind an email address after signing in with Google" width="2860" height="330" data-path="assets/pics/account-security/google-only.png" />

## Special case: the email address matches a different Google account

Consider this configuration:

* **Account 1** has **Email A** under Email Binding.
* **Account 1** already has **Google Account B** under Google Account Binding.
* **Google Account A** uses the same email address as **Email A**.

<Warning>
  If you try to use **Sign in with Google** with Google Account A, sign-in or binding may fail with a message indicating that the account is already linked to another third-party account.
</Warning>

This happens because Account 1 already has Google Account B as its Google credential. The matching email address does not replace an existing Google Account Binding.

To switch from Google Account B to Google Account A:

1. Sign in to Account 1 using Email A or Google Account B.
2. Open **User Center** → **Security Information**.
3. Confirm that Email A is listed under **Email Binding**.
4. Select **Unbind** next to Google Account B.
5. Select **Bind Google Account**, then authorize Google Account A.

If Google Account A is already bound to another Kimi Platform account, it cannot be bound to Account 1 until it is removed from the other account. Kimi Platform does not merge accounts automatically based only on matching email addresses.

## Unbind a Google account

Before unbinding your Google account, make sure an email address is configured under **Email Binding**. If Google is your only sign-in method, select **Bind Email** and complete email verification first.

Then select **Unbind** under **Google Account Binding**. After unbinding, you can no longer use that Google identity to sign in to the account unless you bind it again.

<Warning>
  **Do not remove your only available sign-in method.** Keep a verified email address bound before disconnecting Google access.
</Warning>

## Frequently asked questions

### Is email verification with a Gmail address the same as Sign in with Google?

No. Email verification proves access to an email inbox. **Sign in with Google** authorizes a Google identity through OAuth. They are managed as separate bindings even when the displayed email address is the same.

### Does changing Email Binding change Google Account Binding?

No. Updating the bound email address does not automatically replace or remove the connected Google account. Manage each binding separately in User Center.

### Does a matching email address automatically merge two accounts?

No. Kimi Platform does not merge accounts solely because an Email Binding and a Google identity display the same address. Existing account ownership and third-party bindings are checked before a Google identity can be connected.
