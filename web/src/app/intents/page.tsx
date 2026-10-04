import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { IntentsPage } from "@/portal/intents/IntentsPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.intents") };
}

export default function Page() {
  return <IntentsPage />;
}
