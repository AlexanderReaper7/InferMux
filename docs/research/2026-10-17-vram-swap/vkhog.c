// vkhog: a stand-in for a game's VRAM. Allocates device-local Vulkan memory in
// chunks until a target or the first failure, optionally with
// VK_EXT_pageable_device_local_memory and a priority (what vkd3d-proton and
// DXVK enable when the driver offers it), then holds it, filling every buffer
// once a second and printing the fill bandwidth and the heap budget. A buffer
// the driver moved to system memory shows as a drop from ~600 GB/s to PCIe
// speed. Throwaway measurement tool for docs/research/2026-10-17-vram-swap.md.
//
// usage: vkhog <target_mb> <chunk_mb> <pageable 0|1> <priority 0..1> <hold_s>
#include <vulkan/vulkan.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define CHECK(x) do { VkResult r_ = (x); if (r_ != VK_SUCCESS) { fprintf(stderr, "%s = %d\n", #x, r_); exit(1); } } while (0)

static double now(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec + ts.tv_nsec / 1e9;
}

static VkPhysicalDevice phys;

static void budget(const char *tag) {
    VkPhysicalDeviceMemoryBudgetPropertiesEXT b = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_MEMORY_BUDGET_PROPERTIES_EXT};
    VkPhysicalDeviceMemoryProperties2 p = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_MEMORY_PROPERTIES_2, .pNext = &b};
    vkGetPhysicalDeviceMemoryProperties2(phys, &p);
    printf("%8.3f %s", now(), tag);
    for (uint32_t h = 0; h < p.memoryProperties.memoryHeapCount; h++)
        printf(" heap%u%s budget=%lluMB usage=%lluMB", h,
               (p.memoryProperties.memoryHeaps[h].flags & VK_MEMORY_HEAP_DEVICE_LOCAL_BIT) ? "(dev)" : "(sys)",
               (unsigned long long)(b.heapBudget[h] >> 20), (unsigned long long)(b.heapUsage[h] >> 20));
    printf("\n");
    fflush(stdout);
}

int main(int argc, char **argv) {
    if (argc < 6) {
        fprintf(stderr, "usage: vkhog <target_mb> <chunk_mb> <pageable 0|1> <priority 0..1> <hold_s>\n");
        return 2;
    }
    uint64_t target = strtoull(argv[1], 0, 10), chunk = strtoull(argv[2], 0, 10);
    int pageable = atoi(argv[3]);
    float prio = atof(argv[4]);
    int hold = atoi(argv[5]);
    setvbuf(stdout, NULL, _IOLBF, 0);

    VkApplicationInfo app = {.sType = VK_STRUCTURE_TYPE_APPLICATION_INFO, .apiVersion = VK_API_VERSION_1_3};
    VkInstanceCreateInfo ici = {.sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO, .pApplicationInfo = &app};
    VkInstance inst;
    CHECK(vkCreateInstance(&ici, 0, &inst));
    uint32_t n = 1;
    vkEnumeratePhysicalDevices(inst, &n, &phys);

    VkPhysicalDeviceMemoryProperties mp;
    vkGetPhysicalDeviceMemoryProperties(phys, &mp);
    int type = -1;
    for (uint32_t i = 0; i < mp.memoryTypeCount; i++) {
        VkMemoryPropertyFlags f = mp.memoryTypes[i].propertyFlags;
        if ((f & VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT) && !(f & VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT)) { type = i; break; }
    }
    printf("memory type %d heap %u\n", type, mp.memoryTypes[type].heapIndex);

    float qp = 1.0f;
    VkDeviceQueueCreateInfo qci = {.sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO, .queueFamilyIndex = 0, .queueCount = 1, .pQueuePriorities = &qp};
    VkPhysicalDevicePageableDeviceLocalMemoryFeaturesEXT pf = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PAGEABLE_DEVICE_LOCAL_MEMORY_FEATURES_EXT, .pageableDeviceLocalMemory = VK_TRUE};
    const char *ext[3] = {"VK_EXT_memory_budget", "VK_EXT_memory_priority", "VK_EXT_pageable_device_local_memory"};
    VkDeviceCreateInfo dci = {.sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO, .queueCreateInfoCount = 1, .pQueueCreateInfos = &qci,
                              .enabledExtensionCount = pageable ? 3 : 1, .ppEnabledExtensionNames = ext, .pNext = pageable ? &pf : 0};
    VkDevice dev;
    CHECK(vkCreateDevice(phys, &dci, 0, &dev));
    VkQueue q;
    vkGetDeviceQueue(dev, 0, 0, &q);
    VkCommandPool pool;
    VkCommandPoolCreateInfo pci = {.sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO, .flags = VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT, .queueFamilyIndex = 0};
    CHECK(vkCreateCommandPool(dev, &pci, 0, &pool));
    VkCommandBuffer cb;
    VkCommandBufferAllocateInfo cbai = {.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO, .commandPool = pool, .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY, .commandBufferCount = 1};
    CHECK(vkAllocateCommandBuffers(dev, &cbai, &cb));
    VkFence fence;
    VkFenceCreateInfo fci = {.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO};
    CHECK(vkCreateFence(dev, &fci, 0, &fence));

    budget("start");
    size_t max = target / chunk + 1, count = 0;
    VkDeviceMemory *mem = calloc(max, sizeof *mem);
    VkBuffer *buf = calloc(max, sizeof *buf);
    double t0 = now();
    while (count * chunk < target) {
        VkMemoryPriorityAllocateInfoEXT pr = {.sType = VK_STRUCTURE_TYPE_MEMORY_PRIORITY_ALLOCATE_INFO_EXT, .priority = prio};
        VkMemoryAllocateInfo mai = {.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO, .allocationSize = chunk << 20, .memoryTypeIndex = type, .pNext = pageable ? &pr : 0};
        VkResult r = vkAllocateMemory(dev, &mai, 0, &mem[count]);
        if (r != VK_SUCCESS) {
            printf("%8.3f allocation %zu (%llu MB so far) failed: VkResult %d\n", now(), count, (unsigned long long)(count * chunk), r);
            break;
        }
        VkBufferCreateInfo bci = {.sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO, .size = chunk << 20,
                                  .usage = VK_BUFFER_USAGE_TRANSFER_DST_BIT | VK_BUFFER_USAGE_STORAGE_BUFFER_BIT};
        CHECK(vkCreateBuffer(dev, &bci, 0, &buf[count]));
        CHECK(vkBindBufferMemory(dev, buf[count], mem[count], 0));
        count++;
    }
    printf("%8.3f allocated %zu x %llu MB = %llu MB in %.2f s\n", now(), count, (unsigned long long)chunk,
           (unsigned long long)(count * chunk), now() - t0);
    budget("allocated");

    for (int s = 0; s <= hold; s++) {
        VkCommandBufferBeginInfo bi = {.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO, .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT};
        CHECK(vkBeginCommandBuffer(cb, &bi));
        for (size_t i = 0; i < count; i++) vkCmdFillBuffer(cb, buf[i], 0, VK_WHOLE_SIZE, (uint32_t)s);
        CHECK(vkEndCommandBuffer(cb));
        VkSubmitInfo si = {.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO, .commandBufferCount = 1, .pCommandBuffers = &cb};
        double f0 = now();
        VkResult r = vkQueueSubmit(q, 1, &si, fence);
        if (r == VK_SUCCESS) r = vkWaitForFences(dev, 1, &fence, VK_TRUE, 30ull * 1000000000ull);
        double dt = now() - f0;
        if (r != VK_SUCCESS) {
            printf("%8.3f fill failed: VkResult %d\n", now(), r);
            break;
        }
        vkResetFences(dev, 1, &fence);
        vkResetCommandBuffer(cb, 0);
        char tag[96];
        snprintf(tag, sizeof tag, "t=%ds fill %llu MB in %.1f ms = %.0f GB/s", s, (unsigned long long)(count * chunk), dt * 1e3,
                 (count * chunk) / 1024.0 / dt);
        budget(tag);
        struct timespec ts = {.tv_sec = 1};
        if (s < hold) nanosleep(&ts, 0);
    }
    for (size_t i = 0; i < count; i++) {
        vkDestroyBuffer(dev, buf[i], 0);
        vkFreeMemory(dev, mem[i], 0);
    }
    budget("freed");
    return 0;
}
