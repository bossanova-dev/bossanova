---
title: Organizations and Members
sidebar_label: Organizations
description: 'Create a Bossanova Cloud organization, invite people, set their roles, remove members, revoke invitations, leave, and delete an organization.'
---

# Organizations and Members

An organization is the group your Bossanova Cloud sessions, notes, webhooks and
billing belong to. You manage organizations and their members in the
[web app](./web.md) under **Settings**, where each organization has its own
group of pages: **General**, **Members**, **Proof pages**, **Billing** and the
rest. The `boss` CLI, the
terminal UI and the MCP tools have no member-management commands. They only
choose which organization a repository belongs to (see
[Which organization a repository's sessions land in](#which-organization-a-repositorys-sessions-land-in)).

**General**, **Members** and **Billing** stay open without Bossanova Cloud
access, so an owner always keeps a way to fix billing or leave. The other organization
pages sit behind it: without an active plan they send you to the subscribe page
instead. [Signing In](./login.md#if-bossanova-cloud-needs-a-subscription)
covers choosing a plan.

## Personal and team organizations

Signing up creates a personal organization that contains only you. Every
account has exactly one, and it stays with the account: nobody leaves or
deletes a personal organization, and its owner is never removed from it.

Every other organization is a team organization. You create it, invite people
to it, and leave or delete it when you are done.

## Roles

Each member has one of two roles. The members table shows them as **Owner**
and **Member**.

| Action                                   | Owner | Member |
| ---------------------------------------- | ----- | ------ |
| See the organization and its member list | Yes   | Yes    |
| Rename the organization                  | Yes   | No     |
| Invite people and revoke invitations     | Yes   | No     |
| Change another member's role             | Yes   | No     |
| Remove another member                    | Yes   | No     |
| Manage the organization's billing        | Yes   | No     |
| Delete the organization                  | Yes   | No     |
| Leave the organization                   | Yes   | Yes    |
| Create a new organization of their own   | Yes   | Yes    |

Owners manage every other row in the members table. Nobody changes their own
role or removes themselves, and an organization always keeps at least one
owner. A member sees the owner-only settings as read-only values, and does not
see **Billing** or **Webhooks** at all.

To rename the organization, an owner edits **Name** on its **General** page
and presses Enter, or moves to another field. Escape puts the old name back.

## Switching organizations

Each organization you belong to has its own group in the Settings sidebar.
Open a page in another organization's group to manage that organization.

## Create an organization

Anyone signed in with Bossanova Cloud access is allowed to create organizations,
and the person who creates one becomes its owner.

1. Open **Settings** in the web app.
2. Click **New organization** at the foot of the sidebar.
3. Enter a name and click **Create organization**. Its **General** page opens.

If the organization has not had billing set up yet, you will be prompted to
subscribe after creating the organization.

## Invite people

Only owners invite people, and the organization needs Bossanova Cloud access.

1. Open the organization's **Members** page.
2. Click **Invite**.
3. Enter one or more email addresses, separated by commas or new lines, and
   choose a role. The role starts as **Member**.
4. Click **Send invites**.

WorkOS, the sign-in provider behind Bossanova Cloud, emails the person a link
to join. They do not need a Bossanova account first. Enter a bare address
such as `ana@example.com`, without a display name in front of it.

Bossanova refuses the invitation if the person is already a member or already
has a pending invitation to this organization. Each organization also has an
invitation rate limit: a burst of 10, refilled at 5 invitations a minute.

### Invitation states

The members table has two bands. **Invited** holds invitations, and **Active**
holds members, with when each joined and was last seen.

| Row under the address           | Meaning                                                                                     |
| ------------------------------- | ------------------------------------------------------------------------------------------- |
| `Invitation sent`               | WorkOS has sent the email and the person has not accepted. The invitation grants no access. |
| `Accepted, waiting for sign-in` | The person accepted in WorkOS but has not signed in to Bossanova yet.                       |

A pending invitation is not a member and does not use a seat. A row waiting for
sign-in has no role or remove controls. It moves to **Active** the first time
the person signs in to Bossanova.

## Change a member's role

Changing roles is owner-only, and only for **Active** users.

1. Open the organization's **Members** page.
2. Click the member's row, or its **⋯** button, and choose **Owner** or
   **Member**.

Nobody changes their own role. To give up ownership, ask another owner to
change your role, or promote someone else and then [leave](#leave-an-organization).
Bossanova refuses any change that would leave the organization with no owner.

## Remove a member or revoke an invitation

Only owners remove members and revoke invitations.

1. Open the organization's **Members** page.
2. Click the member's row and choose the item that removes them from the
   organization, then confirm. On an invitation, click **Revoke** or choose **Revoke invitation**.

A removed member loses access to the organization straight away. A revoked
invitation's link stops working. Your own row offers **Leave** followed by the organization's name
instead: see [leave the organization](#leave-an-organization).

## Leave an organization

Any member of a team organization leaves it from the settings page, owners
included.

1. Open the organization's **General** page.
2. In the **Danger zone**, click **Leave**.
3. Confirm with **Leave organization**.

You lose access to everything in the organization until somebody invites you
back. The web app then opens your Preferences.

The last owner of an organization is not allowed to leave, even when nobody
else is in it. Promote another member to **Owner** first, or
[delete the organization](#delete-an-organization).

## Delete an organization

Deleting an organization is owner-only. Bossanova deletes an organization only
once it is empty:

- No members other than you. Remove them first.
- No sessions. Delete them first.
- No Bossanova Cloud subscription that is active or still being activated.
  Cancel it first from the organization's **Billing** page.

Bossanova checks each condition in turn and names the first one that blocks
the delete.

1. Open the organization's **General** page.
2. In the **Danger zone**, click **Delete…**.
3. Type the organization's name and click **Delete organization**.

Deleting removes the organization for everybody and cannot be undone.

## Seats and billing

A paid Bossanova Cloud plan bills an organization per seat, and each active
member is one seat. Pending invitations do not count. When someone accepts an
invitation and signs in, is removed, or leaves, Bossanova sets the
subscription's seat quantity to the new member count. Stripe prorates the
change for the rest of the billing period. A subscription never drops below one
seat.

Owners manage the subscription on the organization's **Billing** page: it shows
the plan, its seats and renewal, and **Manage** and **Open** open the Stripe
billing portal.

## Which organization a repository's sessions land in

Each repository is mapped to at most one organization. Set the mapping in the
terminal UI's repository settings, or in the **Organization** field on the
repository's edit page in the web app. Any member of an organization maps a
repository to it, unless another organization has already claimed that
repository.

A mapped repository sends its sessions and notes to that organization, and an
unmapped one sends them to your personal organization.

## Using the API

The web app is built on the organization calls in the
`bossanova.v1.OrchestratorService` API. The [API Reference](/reference/api)
lists every request and response field.

| RPC                         | Who                 | What it does                                                                |
| --------------------------- | ------------------- | --------------------------------------------------------------------------- |
| `ListOrganizations`         | Any signed-in user  | Lists the organizations you belong to, with your role and the member count. |
| `GetOrganization`           | Member              | Returns one organization. `organization_id` is required.                    |
| `CreateOrganization`        | Any user with Cloud | Creates an organization and makes you its owner.                            |
| `RenameOrganization`        | Owner               | Renames the organization.                                                   |
| `ListOrganizationMembers`   | Member              | Lists members, pending invitations and invitations waiting for sign-in.     |
| `InviteOrganizationMember`  | Owner               | Sends an invitation for an email address and role.                          |
| `SetOrganizationMemberRole` | Owner               | Sets another member's role to `MEMBER_ROLE_OWNER` or `MEMBER_ROLE_USER`.    |
| `RemoveOrganizationMember`  | Owner               | Removes another member, or revokes a pending invitation by `invitation_id`. |
| `LeaveOrganization`         | Member              | Ends your own membership.                                                   |
| `DeleteOrganization`        | Owner               | Deletes an empty team organization.                                         |

`CreateOrganization` and `ListOrganizations` take no `organization_id`.
`GetOrganization` requires one. The other calls take an `organization_id`, and
an empty value acts on your active organization:

- `ListOrganizationMembers` and `LeaveOrganization` accept any organization you
  belong to.
- The owner calls (`RenameOrganization`, `InviteOrganizationMember`,
  `SetOrganizationMemberRole`, `RemoveOrganizationMember` and
  `DeleteOrganization`) act only on your active organization. A different
  `organization_id` is refused with `organization membership required`. Switch
  your active organization first.

Revoking an invitation through `RemoveOrganizationMember` needs the
`Bossanova-Version` header set to `2026-09-11` or later. Older versions refuse
an `invitation_id`. See the [API changelog](../reference/api-changelog.mdx).

## Troubleshooting

The web app shows the server's refusal message word for word. Find it in the
first column.

| Message                                                                                          | What to do                                                                                                   |
| ------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------ |
| `organization name is required`                                                                  | Enter a name with at least one non-space character.                                                          |
| `organization name is too long`                                                                  | Shorten the name to 120 bytes or less.                                                                       |
| `a valid email address is required`                                                              | Enter a bare address such as `ana@example.com`, without a display name.                                      |
| `that person is already a member of this organization or has a pending invitation`               | Check the members table. Revoke the old invitation first if you need to send a new one.                      |
| `organization invite rate limit exceeded`                                                        | Wait a minute. The organization regains 5 invitations a minute, up to 10.                                    |
| `a member cannot change their own role`                                                          | Ask another owner to change it.                                                                              |
| `a member cannot remove themselves from an organization`                                         | Use **Leave organization** in the **Danger zone**.                                                           |
| `an organization must keep at least one owner`                                                   | Promote another member to **Owner** first, or delete the organization if you are its only member.            |
| `a personal organization cannot be left`                                                         | Personal organizations stay with your account. Leave team organizations only.                                |
| `a personal organization cannot be deleted`                                                      | Personal organizations stay with your account. Delete team organizations only.                               |
| `a user cannot be removed from their own personal organization`                                  | The owner of a personal organization stays in it.                                                            |
| `this organization still has members -- remove them before deleting it`                          | Remove every other member, then delete.                                                                      |
| `this organization still has sessions -- delete them before deleting it`                         | Delete the organization's sessions, then delete.                                                             |
| `this organization still has a cloud subscription -- cancel it before deleting the organization` | Cancel the subscription with the billing button on the page, then delete.                                    |
| `cannot confirm this organization is empty right now`                                            | Bossanova could not check the organization's contents. Try again later.                                      |
| `organization owner role required`                                                               | Ask an owner of the organization to do it.                                                                   |
| `organization membership required`                                                               | You are not a member, or an owner call named an organization other than your active one. Switch to it first. |
| `the organization directory is temporarily unavailable`                                          | WorkOS did not respond. Try again later.                                                                     |

If creating an organization or sending an invitation fails with a message
about your Bossanova Cloud plan, the account has no active Cloud access. See
[If Bossanova Cloud needs a subscription](./login.md#if-bossanova-cloud-needs-a-subscription).
