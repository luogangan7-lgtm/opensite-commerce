/** Required #366 evidence; admin spec checks both viewports after rendering.
 * Depends only on snapshot names; provenance and artifact authority remain in the existing CI pipeline. */
export const notificationVisualStates = {
 lastPage:'admin-notifications-last-page.png',
 longMetadata:'admin-notifications-long-metadata.png',
 longMessage:'admin-notifications-long-message.png',
 longTime:'admin-notifications-long-time.png',
 empty:'admin-notifications-empty.png',
 listError:'admin-notifications-list-error.png',
 failed:'admin-notifications-failed.png',
 escapeFocus:'admin-notifications-escape-focus.png',
} as const;
export const requiredNotificationSnapshots = ['desktop','mobile'].flatMap(viewport=>
 Object.values(notificationVisualStates).map(name=>`${viewport}/${name}`));
export function missingNotificationSnapshots(snapshots:Iterable<string>){
 const present=new Set(snapshots);
 return requiredNotificationSnapshots.filter(snapshot=>!present.has(snapshot));
}
