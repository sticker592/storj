// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

import Vue from 'vue';
import App from './App.vue';
import router from './router';
import store from './store';

declare module 'vue/types/vue' {
    interface Vue {
        analytics: {
            page(name: string): void,
            track(event: string): void,
            identity(): void,
        };
    }
}

Vue.config.productionTip = false;
Vue.prototype.analytics = (<any>window).analytics;

new Vue({
    router,
    store,
    render: (h) => h(App),
}).$mount('#app');
