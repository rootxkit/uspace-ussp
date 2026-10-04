import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { IntentFormPage } from "@/portal/intents/IntentFormPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.new_intent") };
}

export default function Page() {
  return <IntentFormPage />;
}
