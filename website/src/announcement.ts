/*
 * The announcement bar at the very top of every page (landing page and docs).
 * Set `announcement` to show one; leave it null for no bar.
 *
 *   export const announcement: Announcement | null = {
 *   	text: 'agtd v1.0 has been released!',
 *   	href: 'https://github.com/robmorgan/agtd/releases',
 *   	linkLabel: 'Release notes',
 *   };
 *
 * Keep the text to one short line: the bar is a single row and long text is
 * cut with an ellipsis on narrow screens.
 */
export interface Announcement {
	text: string;
	/** Where the bar's link goes. Omit for a bar with no link. */
	href?: string;
	/** The link's words. Defaults to "Learn more". */
	linkLabel?: string;
}

export const announcement: Announcement | null = null;
