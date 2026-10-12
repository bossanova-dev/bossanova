---
title: Organizations and Members
sidebar_label: Organizations
description: 'Create a Bossanova Cloud organization, invite people, set their roles, remove members, revoke invitations, leave, and delete an organization.'
---

# Organizations and Members

An organization is the group your Bossanova Cloud sessions, notes, webhooks and
billing belong to. You manage organizations and their members in the
[web app](./web.md) under **Settings → Organization**. The `boss` CLI, the
terminal UI and the MCP tools have no member-management commands. They only
choose which organization a repository belongs to (see
[Which organization a repository's sessions land in](#which-organization-a-repositorys-sessions-land-in)).

The Organization settings page sits behind Bossanova Cloud access. If your
account has no active plan, the web app sends you to the subscribe page
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
and **User**.

| Action                                   | Owner | User |
| ---------------------------------------- | ----- | ---- |
| See the organization and its member list | Yes   | Yes  |
| Rename the organization                  | Yes   | No   |
| Invite people and revoke invitations     | Yes   | No   |
| Change another member's role             | Yes   | No   |
| Remove another member                    | Yes   | No   |
| Manage the organization's billing        | Yes   | No   |
| Delete the organization                  | Yes   | No   |
| Leave the organization                   | Yes   | Yes  |
| Create a new organization of their own   | Yes   | Yes  |

Owners manage every other row in the members table. Nobody changes their own
role or removes themselves, and an organization always keeps at least one
owner.

To rename the organization, an owner clicks its name at the top of the page,
types the new name and presses Enter. Escape, or clicking anywhere else,
cancels the edit.

## Switching organizations

When you belong to more than one organization, the Organization settings page
shows an organization picker next to the **New organization** button. Choose
an organization to open its settings.

## Create an organization

Anyone signed in with Bossanova Cloud access is allowed to create organizations,
and the person who creates one becomes its owner.

1. Open **Settings → Organization** in the web app.
2. Click **New organization**.
3. Enter a name and click **Create organization**.

If the organization has not had billing set up yet, you will be prompted to
subscribe after creating the organization.

## Invite people

Only owners invite people, and the organization needs Bossanova Cloud access.

1. Open **Settings → Organization** for the organization.
2. Click **Invite member**.
3. Enter the person's email address and choose a role. The role starts as
   **User**.
4. Click **Invite member**.

WorkOS, the sign-in provider behind Bossanova Cloud, emails the person a link
to join. They do not need a Bossanova account first. Enter a bare address
such as `ana@example.com`, without a display name in front of it.

Bossanova refuses the invitation if the person is already a member or already
has a pending invitation to this organization. Each organization also has an
invitation rate limit: a burst of 10, refilled at 5 invitations a minute.

### Invitation states

The **Status** column in the members table tracks each invitation.

| Status                               | Meaning                                                                                     |
| ------------------------------------ | ------------------------------------------------------------------------------------------- |
| `Invite pending`                     | WorkOS has sent the email and the person has not accepted. The invitation grants no access. |
| `Invite accepted — awaiting sign-in` | The person accepted in WorkOS but has not signed in to Bossanova yet.                       |
| `Active`                             | The person is a member, with the role shown in the **Role** column.                         |

A pending invitation is not a member and does not use a seat. A row waiting for
sign-in has no role or remove controls. It becomes an **Active** member row the
first time the person signs in to Bossanova.

## Change a member's role

Changing roles is owner-only, and only for **Active** users.

1. Open **Settings → Organization**.
2. In the member's **Role** column, choose **Owner** or **User**.

Your own row has no role control. To give up ownership, ask another owner to
change your role, or promote someone else and then [leave](#leave-an-organization).
Bossanova refuses any change that would leave the organization with no owner.

## Remove a member or revoke an invitation

Only owners remove members and revoke invitations.

1. Open **Settings → Organization**.
2. On the member's row, click the remove button. On an `Invite pending` row,
   the button reads **Revoke invitation**.
3. Confirm in the **Remove member?** or **Revoke invitation?** dialog.

A removed member loses access to the organization straight away. A revoked
invitation's link stops working. Your own row has no remove button: to give up
your own membership, [leave the organization](#leave-an-organization) instead.

## Leave an organization

Any member of a team organization leaves it from the settings page, owners
included.

1. Open **Settings → Organization** for the organization.
2. In the **Danger zone**, click **Leave organization**.
3. Confirm in the **Leave organization?** dialog.

You lose access to everything in the organization until somebody invites you
back. The web app then returns you to the sessions list.

The last owner of an organization is not allowed to leave, even when nobody
else is in it. Promote another member to **Owner** first, or
[delete the organization](#delete-an-organization).

## Delete an organization

Deleting an organization is owner-only. Bossanova deletes an organization only
once it is empty:

- No members other than you. Remove them first.
- No sessions. Delete them first.
- No Bossanova Cloud subscription that is active or still being activated.
  Cancel it first with the `Manage billing for <organization name>` button on
  the same page.

Bossanova checks each condition in turn and names the first one that blocks
the delete.

1. Open **Settings → Organization** for the organization.
2. In the **Danger zone**, click **Delete organization**.
3. Confirm in the **Delete organization?** dialog.

Deleting removes the organization for everybody and cannot be undone.

## Seats and billing

A paid Bossanova Cloud plan bills an organization per seat, and each active
member is one seat. Pending invitations do not count. When someone accepts an
invitation and signs in, is removed, or leaves, Bossanova sets the
subscription's seat quantity to the new member count. Stripe prorates the
change for the rest of the billing period. A subscription never drops below one
seat.

Owners manage the subscription via the **Billing** section in the Organization
settings page.

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
