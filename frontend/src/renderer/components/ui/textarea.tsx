import * as React from "react";
import { cn } from "../../lib/utils";

/**
 * Textarea is the multi-line sibling of Input, styled identically so a form
 * that mixes the two reads as one control set.
 *
 * It exists because a Task specification is prose: sections, blank lines, a
 * list of acceptance criteria. Before P2-E the objective was a single-line
 * <input>, which does not merely look cramped — a browser strips the newlines
 * when you paste multi-line text into one, so a structured brief silently
 * arrived as a run-on paragraph.
 *
 * `field-sizing-content` lets the control grow with what is typed where the
 * browser supports it, and max-height keeps it from swallowing the form; past
 * that it scrolls. `resize-y` leaves the reader in control, because how much
 * of a long specification to look at is their call and not the layout's.
 */
export const Textarea = React.forwardRef<
	HTMLTextAreaElement,
	React.TextareaHTMLAttributes<HTMLTextAreaElement>
>(({ className, ...props }, ref) => (
	<textarea
		data-slot="textarea"
		className={cn(
			"field-sizing-content min-h-32 max-h-[60vh] w-full min-w-0 resize-y rounded-md border border-transparent bg-input/50 px-3 py-2 text-sm text-foreground transition-[color,box-shadow,background-color] outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30 disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40",
			className,
		)}
		ref={ref}
		{...props}
	/>
));

Textarea.displayName = "Textarea";
