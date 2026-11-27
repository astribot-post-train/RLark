import { iamPortalEnabled } from "./iam/portal";
import astribotDark from "./iam/assets/astribot-logo-dark.png";
import astribotLight from "./iam/assets/astribot-logo-light.png";

const astribotBrand = {
  name: "Astribot",
  lightLogo: astribotLight,
  darkLogo: astribotDark,
  sidebarClass: "brand-astribot",
  footerClass: "platform-footer-astribot",
};

export function consoleBrand() {
  return iamPortalEnabled() ? astribotBrand : undefined;
}
