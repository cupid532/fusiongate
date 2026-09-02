import * as React from "react"
import * as DialogPrimitive from "@radix-ui/react-dialog"
import { X } from "lucide-react"

import { cn } from "@/lib/utils"

const Dialog = DialogPrimitive.Root
const DialogPortal = DialogPrimitive.Portal

const DialogOverlay = React.forwardRef<
  React.ElementRef<typeof DialogPrimitive.Overlay>,
  React.ComponentPropsWithoutRef<typeof DialogPrimitive.Overlay>
>(({ className, ...props }, ref) => (
  <DialogPrimitive.Overlay
    ref={ref}
    className={cn("fixed inset-0 z-50 bg-black/40 backdrop-blur-sm", className)}
    {...props}
  />
))
DialogOverlay.displayName = DialogPrimitive.Overlay.displayName

const DialogContent = React.forwardRef<
  React.ElementRef<typeof DialogPrimitive.Content>,
  React.ComponentPropsWithoutRef<typeof DialogPrimitive.Content>
>(({ className, children, ...props }, ref) => (
  <DialogPortal>
    <DialogOverlay />
    <DialogPrimitive.Content
      ref={ref}
      className={cn(
        "fixed left-1/2 top-1/2 z-50 grid max-h-[90vh] w-[calc(100vw-2rem)] max-w-lg -translate-x-1/2 -translate-y-1/2 gap-4 overflow-y-auto border bg-background p-5 shadow-lg sm:w-full sm:rounded-xl sm:p-6",
        className
      )}
      {...props}
    >
      {/*
        The close button, pinned to the top-right in every dialog.

        Two constraints make this fiddlier than `absolute right-4 top-4`:

        1. By default this content box is itself the scroll container (max-h +
           overflow-y-auto). An absolutely positioned child of a scroll
           container scrolls away with the content, so in a tall dialog — the
           provider editor, say — the button used to vanish off the top.
           `sticky top-0` pins it to the visible edge instead.
        2. Consumers may override the layout: ProviderModelManagementDialog
           passes `flex flex-col overflow-hidden` and scrolls an inner panel.
           So this cannot rely on grid-only properties (an earlier version used
           `justify-self-end`, which is inert in a flex column and dropped the
           button to the bottom-left), and it must come FIRST in DOM order so
           it lands at the top of a flex column rather than after the footer.

        `h-0` plus the negative bottom margin cancel the parent's `gap`, so a
        zero-height row does not push the header down.
      */}
      <div className="pointer-events-none sticky top-0 z-20 -mb-4 flex h-0 justify-end">
        <DialogPrimitive.Close className="pointer-events-auto -mr-1 -mt-1 grid h-7 w-7 place-items-center rounded-md bg-background/80 text-muted-foreground opacity-70 backdrop-blur-sm transition-opacity hover:bg-muted hover:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:pointer-events-none">
          <X className="h-4 w-4" />
          <span className="sr-only">关闭</span>
        </DialogPrimitive.Close>
      </div>
      {children}
    </DialogPrimitive.Content>
  </DialogPortal>
))
DialogContent.displayName = DialogPrimitive.Content.displayName

const DialogHeader = ({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) => (
  <div className={cn("flex flex-col space-y-1.5 text-left", className)} {...props} />
)
DialogHeader.displayName = "DialogHeader"

const DialogFooter = ({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) => (
  // `gap-2` instead of `sm:space-x-2`: space-x contributes nothing on the
  // mobile `flex-col-reverse` axis, so stacked buttons sat flush against each
  // other. gap applies on whichever axis is active.
  <div className={cn("flex flex-col-reverse gap-2 sm:flex-row sm:justify-end", className)} {...props} />
)
DialogFooter.displayName = "DialogFooter"

const DialogTitle = React.forwardRef<
  React.ElementRef<typeof DialogPrimitive.Title>,
  React.ComponentPropsWithoutRef<typeof DialogPrimitive.Title>
>(({ className, ...props }, ref) => (
  <DialogPrimitive.Title ref={ref} className={cn("text-lg font-semibold leading-none tracking-tight", className)} {...props} />
))
DialogTitle.displayName = DialogPrimitive.Title.displayName

const DialogDescription = React.forwardRef<
  React.ElementRef<typeof DialogPrimitive.Description>,
  React.ComponentPropsWithoutRef<typeof DialogPrimitive.Description>
>(({ className, ...props }, ref) => (
  <DialogPrimitive.Description ref={ref} className={cn("text-sm text-muted-foreground", className)} {...props} />
))
DialogDescription.displayName = DialogPrimitive.Description.displayName

export { Dialog, DialogContent, DialogHeader, DialogFooter, DialogTitle, DialogDescription }
